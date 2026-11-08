package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hint"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/internal/tailcat"
)

type peerMember struct {
	peerID       string
	proxyURL     *url.URL
	models       []string
	reverseProxy *httputil.ReverseProxy
	transport    *http.Transport
	tailcat      *tailcat.Client
	apiKey       string
	// upstream is the provider behind the peer, guessed once from its proxy
	// host. hint.Unknown when no detection rule matches.
	upstream hint.Upstream
}

type peerRoute struct {
	member  *peerMember
	modelID string
	logs    *httputil.ReverseProxy
	load    *httputil.ReverseProxy
	unload  *httputil.ReverseProxy
}

// PeerModelStatus is one peer model's lifecycle state as reported by the
// peer's /v1/models listing. State uses the same vocabulary as local model
// state in the UI: ready, starting, stopping, stopped or unknown.
type PeerModelStatus struct {
	State           string
	LoadingProgress *float64
	LoadingMessage  string
}

type Peer struct {
	cfg     config.Config
	logger  *logmon.Monitor
	peers   map[string]*peerRoute
	members []*peerMember

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup
}

func NewPeer(cfg config.Config, logger *logmon.Monitor) (*Peer, error) {
	if err := config.ValidatePeerNamespace(cfg); err != nil {
		return nil, err
	}

	peers := cfg.Peers
	modelMap := make(map[string]*peerRoute)
	bareRoutes := make(map[string][]*peerRoute)

	peerIDs := make([]string, 0, len(peers))
	tailcatClients := make(map[string]*tailcat.Client)
	members := make([]*peerMember, 0, len(peers))
	for peerID := range peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		peer := peers[peerID]
		var tailcatClient *tailcat.Client
		proxyFromEnvironment := http.ProxyFromEnvironment
		dialContext := (&net.Dialer{
			Timeout:   time.Duration(peer.Timeouts.Connect) * time.Second,
			KeepAlive: time.Duration(peer.Timeouts.KeepAlive) * time.Second,
		}).DialContext
		if _, blob, privateKey, found := peer.Tailcat(); found {
			clientKey := "ephemeral:" + peerID
			if privateKey != nil {
				clientKey = privateKey.Identity() + ":" + blob
			}
			tailcatClient = tailcatClients[clientKey]
			if tailcatClient == nil {
				tailcatClient = tailcat.NewClient(peerID, blob, privateKey, logger)
				tailcatClients[clientKey] = tailcatClient
			}
			proxyFromEnvironment = nil
			dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if peer.Timeouts.Connect > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Duration(peer.Timeouts.Connect)*time.Second)
					defer cancel()
				}
				return tailcatClient.DialContext(ctx, network, address)
			}
		}

		// Tailcat needs a new dial per request so a restarted server cannot
		// strand an HTTP request on a connection from the old server.
		peerTransport := &http.Transport{
			Proxy:                 proxyFromEnvironment,
			DialContext:           dialContext,
			DisableKeepAlives:     tailcatClient != nil,
			TLSHandshakeTimeout:   time.Duration(peer.Timeouts.TLSHandshake) * time.Second,
			ResponseHeaderTimeout: time.Duration(peer.Timeouts.ResponseHeader) * time.Second,
			ExpectContinueTimeout: time.Duration(peer.Timeouts.ExpectContinue) * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       time.Duration(peer.Timeouts.IdleConn) * time.Second,
		}

		reverseProxy := &httputil.ReverseProxy{
			Transport: peerTransport,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(peer.ProxyURL)
				r.Out.Host = r.Out.URL.Host
				// The peer is a server-to-server hop. Some APIs (Anthropic)
				// reject requests carrying a browser Origin; see issue #1214.
				swaputil.StripBrowserRequestHeaders(r.Out.Header)
			},
		}

		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			// The peer is another llama-swap that already applied its own CORS
			// policy. ReverseProxy adds rather than replaces headers, so keep
			// only this instance's; see issue #85.
			swaputil.StripUpstreamCORSHeaders(resp.Header)

			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		}

		reverseProxy.ErrorHandler = newPeerErrorHandler(peerID, logger)

		pp := &peerMember{
			peerID:       peerID,
			proxyURL:     peer.ProxyURL,
			reverseProxy: reverseProxy,
			transport:    peerTransport,
			tailcat:      tailcatClient,
			apiKey:       peer.ApiKey,
			upstream:     hint.ForURL(peer.ProxyURL),
		}
		members = append(members, pp)

		seen := make(map[string]struct{})
		for _, modelID := range peer.Models {
			if _, duplicate := seen[modelID]; duplicate {
				continue
			}
			seen[modelID] = struct{}{}
			pp.models = append(pp.models, modelID)

			route := &peerRoute{
				member:  pp,
				modelID: modelID,
				logs:    newPeerLogProxy(pp, modelID, logger),
				load:    newPeerControlProxy(pp, "load", modelID, logger),
				unload:  newPeerControlProxy(pp, "unload", modelID, logger),
			}
			modelMap[config.PeerModelFQN(peerID, modelID)] = route
			bareRoutes[modelID] = append(bareRoutes[modelID], route)
		}
	}

	for modelID, routes := range bareRoutes {
		if len(routes) != 1 {
			continue
		}
		if _, reserved := modelMap[modelID]; reserved {
			continue
		}
		modelMap[modelID] = routes[0]
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())

	r := &Peer{
		cfg:         cfg,
		logger:      logger,
		peers:       modelMap,
		members:     members,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}
	return r, nil
}

// newPeerErrorHandler reports a peer proxy failure. It is shared by the
// inference reverse proxy and the log stream proxy.
func newPeerErrorHandler(peerID string, logger *logmon.Monitor) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// A cancelled request is not a peer failure, so keep it out of the
		// warning stream whether or not the sentinel applies below.
		if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
			logger.Debugf("peer %s: request cancelled: %v", peerID, err)
		} else {
			logger.Warnf("peer %s: proxy error: %v", peerID, err)
		}

		// Only a client that actually hung up gets the recorded-only
		// sentinel (#1029). A request cancelled server-side still has a
		// client waiting for an answer.
		if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
			return
		}

		errMsg := fmt.Sprintf("peer proxy error: %v", err)
		if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
			errMsg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, errMsg)
	}
}

// newPeerLogProxy builds the reverse proxy for a peer model's own
// GET /logs/stream/<model> endpoint. Requests keep their query string (notably
// ?no-history) but are rewritten from the local peer-scoped model name to the
// peer's real model id.
func newPeerLogProxy(member *peerMember, modelID string, logger *logmon.Monitor) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: member.transport,
		// Logs are a live tail; flush each chunk instead of buffering it.
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(member.proxyURL)
			base := strings.TrimSuffix(member.proxyURL.Path, "/")
			r.Out.URL.Path = base + "/logs/stream/" + modelID
			r.Out.URL.RawPath = ""
			r.Out.Host = r.Out.URL.Host
			swaputil.StripBrowserRequestHeaders(r.Out.Header)
			if member.apiKey != "" {
				r.Out.Header.Set("Authorization", "Bearer "+member.apiKey)
				r.Out.Header.Set("x-api-key", member.apiKey)
			}
		},
		ErrorHandler: newPeerErrorHandler(member.peerID, logger),
	}
}

// newPeerControlProxy builds a reverse proxy for one lifecycle action
// (/api/models/load or /api/models/unload) on a peer model. The local server
// exposes the same HTTP contract as the peer, so the handler can relay the
// request and its response body unchanged.
func newPeerControlProxy(member *peerMember, action, modelID string, logger *logmon.Monitor) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: member.transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(member.proxyURL)
			base := strings.TrimSuffix(member.proxyURL.Path, "/")
			r.Out.URL.Path = base + "/api/models/" + action + "/" + modelID
			r.Out.URL.RawPath = ""
			r.Out.Host = r.Out.URL.Host
			swaputil.StripBrowserRequestHeaders(r.Out.Header)
			if member.apiKey != "" {
				r.Out.Header.Set("Authorization", "Bearer "+member.apiKey)
				r.Out.Header.Set("x-api-key", member.apiKey)
			}
		},
		ErrorHandler: newPeerErrorHandler(member.peerID, logger),
	}
}

// peerModelsResponse is the subset of a peer's /v1/models response needed to
// mirror its lifecycle state. The status value is loaded/unloaded/starting/
// stopping and loading progress lives under meta.llamaswap.
type peerModelsResponse struct {
	Data []struct {
		ID     string `json:"id"`
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
		Meta struct {
			Llamaswap struct {
				LoadingProgress *float64 `json:"loadingProgress"`
				LoadingMessage  string   `json:"loadingMessage"`
			} `json:"llamaswap"`
		} `json:"meta"`
	} `json:"data"`
}

func peerStateFromAPI(value string) string {
	switch value {
	case "loaded":
		return "ready"
	case "starting":
		return "starting"
	case "stopping":
		return "stopping"
	case "unloaded":
		return "stopped"
	default:
		return "unknown"
	}
}

// fetchStatuses reads a peer's /v1/models listing with the peer's own
// transport and credentials.
func (m *peerMember) fetchStatuses(ctx context.Context) (map[string]PeerModelStatus, error) {
	target := *m.proxyURL
	target.Path = strings.TrimSuffix(m.proxyURL.Path, "/") + "/v1/models"
	target.RawPath = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Host = target.Host
	swaputil.StripBrowserRequestHeaders(req.Header)
	req.Header.Set(swaputil.PeerStatusHeader, "1")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
		req.Header.Set("x-api-key", m.apiKey)
	}

	resp, err := m.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("status: %s", resp.Status)
	}

	var payload peerModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	statuses := make(map[string]PeerModelStatus, len(payload.Data))
	for _, model := range payload.Data {
		statuses[model.ID] = PeerModelStatus{
			State:           peerStateFromAPI(model.Status.Value),
			LoadingProgress: model.Meta.Llamaswap.LoadingProgress,
			LoadingMessage:  model.Meta.Llamaswap.LoadingMessage,
		}
	}
	return statuses, nil
}

// Statuses reports the lifecycle state of every configured peer model. Models
// whose peer cannot be reached are reported as unknown rather than omitted, so
// the caller can replace stale state instead of keeping it forever.
func (r *Peer) Statuses(ctx context.Context) map[string]PeerModelStatus {
	statuses := make(map[string]PeerModelStatus)
	for _, member := range r.members {
		for _, modelID := range member.models {
			statuses[config.PeerModelFQN(member.peerID, modelID)] = PeerModelStatus{State: "unknown"}
		}
	}
	if r.shuttingDown.Load() {
		return statuses
	}

	r.inflight.Add(1)
	defer r.inflight.Done()

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, member := range r.members {
		if len(member.models) == 0 {
			continue
		}
		wg.Add(1)
		go func(member *peerMember) {
			defer wg.Done()
			reported, err := member.fetchStatuses(ctx)
			if err != nil {
				r.logger.Debugf("peer %s: status fetch failed: %v", member.peerID, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, modelID := range member.models {
				if status, ok := reported[modelID]; ok {
					statuses[config.PeerModelFQN(member.peerID, modelID)] = status
				}
			}
		}(member)
	}
	wg.Wait()
	return statuses
}

func (r *Peer) Handles(model string) bool {
	_, ok := r.peers[model]
	return ok
}

func (r *Peer) Shutdown(timeout time.Duration) error {
	if !r.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	if timeout == 0 {
		r.shutdownFn()
		r.inflight.Wait()
		return r.closeTransports(time.Second)
	}

	deadline := time.Now().Add(timeout)
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), deadline)
	defer cancelDeadline()

	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.shutdownFn()
		return r.closeTransports(max(time.Until(deadline), 0))
	case <-deadlineCtx.Done():
		r.shutdownFn()
		select {
		case <-done:
		case <-deadlineCtx.Done():
		}
		return errors.Join(fmt.Errorf("peer shutdown timed out after %v", timeout), r.closeTransports(max(time.Until(deadline), 0)))
	}
}

func (r *Peer) closeTransports(timeout time.Duration) error {
	type closeResult struct {
		index int
		err   error
	}

	deadline := time.Now().Add(timeout)
	closedTailcat := make(map[*tailcat.Client]struct{})
	errs := make([]error, len(r.members))
	done := make(chan closeResult, len(r.members))
	for i, member := range r.members {
		closeTailcat := false
		if member.tailcat != nil {
			if _, closed := closedTailcat[member.tailcat]; !closed {
				closedTailcat[member.tailcat] = struct{}{}
				closeTailcat = true
			}
		}

		go func() {
			var err error
			member.transport.CloseIdleConnections()
			if closeTailcat {
				if closeErr := member.tailcat.CloseWithTimeout(max(time.Until(deadline), 0)); closeErr != nil {
					err = fmt.Errorf("peer %s: %w", member.peerID, closeErr)
				}
			}
			done <- closeResult{index: i, err: err}
		}()
	}

	timer := time.NewTimer(max(time.Until(deadline), 0))
	defer timer.Stop()
	for range r.members {
		select {
		case result := <-done:
			errs[result.index] = result.err
		case <-timer.C:
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

// LogStream proxies a peer model's process log stream. The local UI asks for
// /logs/stream/<peer>/<model>, while the peer serves /logs/stream/<model>. It
// reports whether model belongs to a peer; false means the caller should treat
// the id as unknown.
func (r *Peer) LogStream(w http.ResponseWriter, req *http.Request, model string) bool {
	route, ok := r.peers[model]
	if !ok {
		return false
	}
	if r.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return true
	}

	r.inflight.Add(1)
	defer r.inflight.Done()

	ctx, cancel := context.WithCancel(req.Context())
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	defer stopShutdown()
	defer cancel()

	route.logs.ServeHTTP(w, req.WithContext(ctx))
	return true
}

// Load asks the peer to load one of its models and relays the peer's response.
// It reports whether model belongs to a peer.
func (r *Peer) Load(w http.ResponseWriter, req *http.Request, model string) bool {
	return r.serveControl(w, req, model, true)
}

// Unload asks the peer to stop one of its models and relays the peer's
// response. It reports whether model belongs to a peer.
func (r *Peer) Unload(w http.ResponseWriter, req *http.Request, model string) bool {
	return r.serveControl(w, req, model, false)
}

func (r *Peer) serveControl(w http.ResponseWriter, req *http.Request, model string, load bool) bool {
	route, ok := r.peers[model]
	if !ok {
		return false
	}
	if r.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return true
	}

	r.inflight.Add(1)
	defer r.inflight.Done()

	ctx, cancel := context.WithCancel(req.Context())
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	defer stopShutdown()
	defer cancel()

	target := route.unload
	if load {
		target = route.load
	}
	target.ServeHTTP(w, req.WithContext(ctx))
	return true
}

func (r *Peer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Done()

	data, err := swaputil.FetchContext(req, r.cfg)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	route, found := r.peers[data.ModelID]
	if !found {
		r.logger.Warnf("peer model not found: %s", data.ModelID)
		swaputil.SendError(w, req, ErrNoPeerModelFound)
		return
	}
	pp := route.member

	r.logger.Debugf("peer: routing model %s to peer %s as %s", data.ModelID, pp.peerID, route.modelID)

	if h := hint.From(req.Context()); h != nil {
		h.Route = hint.RoutePeer
		h.PeerID = pp.peerID
		h.Upstream = pp.upstream
	}

	if data.Model != route.modelID {
		req, err = swaputil.ReplaceRequestModel(req, data.Model, route.modelID)
		if err != nil {
			swaputil.SendResponse(w, req, http.StatusBadRequest, err.Error())
			return
		}
	}

	if pp.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+pp.apiKey)
		req.Header.Set("x-api-key", pp.apiKey)
	}

	// Cancel the proxy request when the client disconnects or shutdown times out.
	// Deriving from the request covers the client half directly and keeps the
	// request's context values — notably the client context that tells a real
	// disconnect apart from a server-side cancel. AfterFunc links the unrelated
	// shutdown context in without a goroutine leak.
	ctx, cancel := context.WithCancel(req.Context())
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	pp.reverseProxy.ServeHTTP(w, req)

	stopShutdown()
	cancel()
}
