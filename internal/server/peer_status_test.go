package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_PeerLoadUnload(t *testing.T) {
	control := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[]}`))
		default:
			control <- r.Method + " " + r.URL.Path
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		}
	}))
	defer upstream.Close()

	proxyURL, _ := url.Parse(upstream.URL)
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"peer1": {
			Proxy:    upstream.URL,
			ProxyURL: proxyURL,
			Models:   []string{"model-a"},
		},
	}}
	peer, err := router.NewPeer(cfg, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Shutdown(time.Second)

	s := newTestServerWithConfig(cfg, newStubRouter(nil, ""), peer)

	for _, action := range []string{"load", "unload"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/"+action+"/peer1/model-a", nil))
		if w.Code != http.StatusOK || w.Body.String() != "OK" {
			t.Fatalf("%s response = %d %q", action, w.Code, w.Body.String())
		}
		select {
		case got := <-control:
			want := "POST /api/models/" + action + "/model-a"
			if got != want {
				t.Errorf("peer request=%q want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("peer did not receive %s request", action)
		}
	}
}

func TestServer_PeerStatusMerged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"model-a","status":{"value":"loaded"}}]}`))
	}))
	defer upstream.Close()

	proxyURL, _ := url.Parse(upstream.URL)
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"peer1": {
			Proxy:    upstream.URL,
			ProxyURL: proxyURL,
			Models:   []string{"model-a"},
		},
	}}
	peer, err := router.NewPeer(cfg, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Shutdown(time.Second)

	s := newTestServerWithConfig(cfg, newStubRouter(nil, ""), peer)

	changed := make(chan struct{}, 1)
	cancel := event.On(func(e swaputil.PeerStatusChangedEvent) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer cancel()

	s.refreshPeerStatuses()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Error("refresh did not emit PeerStatusChangedEvent")
	}

	var found bool
	for _, model := range s.modelStatus() {
		if model.Id == "peer1/model-a" {
			found = true
			if model.State != "ready" {
				t.Errorf("peer state=%q want ready", model.State)
			}
		}
	}
	if !found {
		t.Fatal("peer model missing from modelStatus")
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var listing struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	for _, model := range listing.Data {
		if model.ID == "peer1/model-a" {
			if model.Status.Value != "loaded" {
				t.Errorf("peer status=%q want loaded", model.Status.Value)
			}
			return
		}
	}
	t.Fatal("peer model missing from /v1/models")
}

func TestServer_PeerStatusUnknownWhenOffline(t *testing.T) {
	proxyURL, _ := url.Parse("http://127.0.0.1:1")
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"peer1": {
			Proxy:    "http://127.0.0.1:1",
			ProxyURL: proxyURL,
			Models:   []string{"model-a"},
		},
	}}
	peer, err := router.NewPeer(cfg, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Shutdown(time.Second)

	s := newTestServerWithConfig(cfg, newStubRouter(nil, ""), peer)
	s.peerStatusMu.Lock()
	s.peerStatus["peer1/model-a"] = router.PeerModelStatus{State: "ready"}
	s.peerStatusMu.Unlock()

	s.refreshPeerStatuses()

	status, ok := s.peerStatusFor("peer1/model-a")
	if !ok || status.State != "unknown" {
		t.Errorf("offline peer status=%+v ok=%v want unknown", status, ok)
	}
}

func TestServer_PeerStatusEqual(t *testing.T) {
	progress := 0.5
	other := 0.5
	if !peerStatusEqual(
		router.PeerModelStatus{State: "ready", LoadingProgress: &progress},
		router.PeerModelStatus{State: "ready", LoadingProgress: &other},
	) {
		t.Error("equal progress values compared unequal")
	}
	if peerStatusEqual(
		router.PeerModelStatus{State: "ready"},
		router.PeerModelStatus{State: "stopped"},
	) {
		t.Error("different states compared equal")
	}
}
