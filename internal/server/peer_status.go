package server

import (
	"context"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	// peerStatusPollInterval is how often each peer's model listing is read.
	// The UI only needs coarse lifecycle state, so this stays light. Load and
	// unload actions trigger an extra refresh immediately, so this only bounds
	// how quickly changes made elsewhere show up.
	peerStatusPollInterval = 10 * time.Second
	// peerStatusTimeout bounds one polling round so an offline peer cannot
	// hold up the next round or server shutdown.
	peerStatusTimeout = 4 * time.Second
)

// peerStatusProvider is implemented by the peer router. It is optional so
// local routers and test doubles do not need peer status support.
type peerStatusProvider interface {
	Statuses(context.Context) map[string]router.PeerModelStatus
}

// peerModelController is implemented by the peer router to forward model
// lifecycle actions to the peer that owns the model.
type peerModelController interface {
	Load(http.ResponseWriter, *http.Request, string) bool
	Unload(http.ResponseWriter, *http.Request, string) bool
}

// initPeerStatus seeds the cache with unknown for every configured peer model.
// The first successful poll replaces the unknown values.
func (s *Server) initPeerStatus() {
	statuses := make(map[string]router.PeerModelStatus)
	for peerID, peer := range s.cfg.Peers {
		for _, modelID := range peer.Models {
			statuses[config.PeerModelFQN(peerID, modelID)] = router.PeerModelStatus{State: "unknown"}
		}
	}
	s.peerStatusMu.Lock()
	s.peerStatus = statuses
	s.peerStatusMu.Unlock()
}

func (s *Server) startPeerStatusPolling() {
	if len(s.cfg.Peers) == 0 {
		return
	}
	done := make(chan struct{})
	s.peerPollDone = done
	go func() {
		defer close(done)
		s.refreshPeerStatuses()

		ticker := time.NewTicker(peerStatusPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.shutdownCtx.Done():
				return
			case <-ticker.C:
				s.refreshPeerStatuses()
			}
		}
	}()
}

// refreshPeerStatuses reads every peer's model listing and emits one model
// status event when anything changed. A peer that is offline is reported as
// unknown, replacing stale ready/stopped values from before the outage.
func (s *Server) refreshPeerStatuses() {
	provider, ok := s.peer.(peerStatusProvider)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, peerStatusTimeout)
	defer cancel()

	statuses := provider.Statuses(ctx)
	if len(statuses) == 0 {
		return
	}

	changed := false
	s.peerStatusMu.Lock()
	if s.peerStatus == nil {
		s.peerStatus = make(map[string]router.PeerModelStatus, len(statuses))
	}
	for modelID, status := range statuses {
		if old, exists := s.peerStatus[modelID]; exists && peerStatusEqual(old, status) {
			continue
		}
		s.peerStatus[modelID] = status
		changed = true
	}
	s.peerStatusMu.Unlock()

	if changed {
		event.Emit(swaputil.PeerStatusChangedEvent{})
	}
}

func (s *Server) peerStatusFor(modelID string) (router.PeerModelStatus, bool) {
	s.peerStatusMu.RLock()
	defer s.peerStatusMu.RUnlock()
	status, ok := s.peerStatus[modelID]
	return status, ok
}

func peerStatusEqual(a, b router.PeerModelStatus) bool {
	if a.State != b.State || a.LoadingMessage != b.LoadingMessage {
		return false
	}
	if (a.LoadingProgress == nil) != (b.LoadingProgress == nil) {
		return false
	}
	if a.LoadingProgress != nil && *a.LoadingProgress != *b.LoadingProgress {
		return false
	}
	return true
}
