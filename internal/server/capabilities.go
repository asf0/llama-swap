package server

import (
	"context"
	"reflect"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/capcompat"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/hint"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// onUpstreamDetected stores what a local process found when it detected its
// upstream on becoming ready. The process does the probing (see
// ProcessCommand.detectUpstream); the server only keeps the result so
// /v1/models can serve it while the model is unloaded.
//
// This runs on the event dispatcher's consumer goroutine, and a consumer that
// falls behind eventually blocks the publisher, so the store write goes to
// its own goroutine and this returns immediately.
func (s *Server) onUpstreamDetected(e capcompat.DetectedEvent) {
	mc, ok := s.cfg.Models[e.ModelID]
	if !ok || mc.Capabilities.DisableAuto {
		return
	}
	// A process started under an older config reports under its old key; the
	// listing reads the current one, so there is nothing useful to store.
	if e.Key != capcompat.LocalKey(e.ModelID, mc) {
		return
	}
	go s.recordDetection(e, mc)
}

// recordDetection caches one detection result and announces a change in
// what the model advertises.
func (s *Server) recordDetection(e capcompat.DetectedEvent, mc config.ModelConfig) {
	// What the model advertised before, so a detection that learns nothing
	// new does not push a redundant model list to every UI client.
	beforeCaps := s.resolveCapabilities(s.shutdownCtx, e.ModelID, mc)
	beforeUpstream := s.capcompat.Upstream(s.shutdownCtx, e.Key)

	if err := s.capcompat.Record(s.shutdownCtx, e.Key, e.Info, e.Err); err != nil {
		// Debug, not warn. llama-swap also fronts image, speech and
		// transcription servers with no capability surface, and a model that
		// stopped again mid-probe is normal. Neither is worth a warning on
		// every model start.
		s.logs.ProxyLogs.Debugf("capcompat: <%s> discovery failed: %v", e.ModelID, err)
		return
	}

	// The listing pushed when this model went ready may predate the
	// detection, so anything new has to announce itself.
	afterCaps := s.resolveCapabilities(s.shutdownCtx, e.ModelID, mc)
	afterUpstream := s.capcompat.Upstream(s.shutdownCtx, e.Key)
	if !reflect.DeepEqual(beforeCaps, afterCaps) || beforeUpstream != afterUpstream {
		event.Emit(swaputil.ModelCapabilitiesChangedEvent{ModelID: e.ModelID})
	}
}

// localUpstream returns the server family last detected for a local model,
// or hint.Unknown when it has not been detected.
func (s *Server) localUpstream(ctx context.Context, modelID string, mc config.ModelConfig) hint.Upstream {
	if mc.Capabilities.DisableAuto {
		return hint.Unknown
	}
	return s.capcompat.Upstream(ctx, capcompat.LocalKey(modelID, mc))
}

// hasServerUI reports whether the backend serves its own web UI at its root.
// A detected upstream is authoritative, but llama-server is also recognized
// from the configured command so the open-model link works before the first
// probe and when no detection result is cached. The reserved passthrough
// endpoints always have a UI.
func hasServerUI(upstream hint.Upstream, modelID string, mc config.ModelConfig) bool {
	if upstream.HasWebUI() || modelID == config.ComfyUIModelID || modelID == config.AudioCppModelID {
		return true
	}
	return strings.Contains(mc.Cmd, "llama-server")
}

// resolveCapabilities returns the capabilities to advertise for a local model:
// what the config sets, with anything it leaves unset filled in from the last
// successful probe, then from pre-load inference.
//
// Configured values win field by field, so a model that hand-sets one field
// still benefits from discovery for the rest. A model with
// capabilities.disableAuto gets exactly what its config says.
func (s *Server) resolveCapabilities(ctx context.Context, modelID string, mc config.ModelConfig) config.ModelCapConfig {
	caps, _ := s.resolveCapabilitiesWithSource(ctx, modelID, mc)
	return caps
}

// resolveCapabilitiesWithSource also reports where the advertised context
// length came from: "config", "discovered" or "cmd". The source is empty when
// nothing supplied a context length.
func (s *Server) resolveCapabilitiesWithSource(ctx context.Context, modelID string, mc config.ModelConfig) (config.ModelCapConfig, string) {
	caps := mc.Capabilities
	source := ""
	if caps.Context > 0 {
		source = "config"
	}
	if caps.DisableAuto {
		return caps, source
	}

	key := capcompat.LocalKey(modelID, mc)
	auto, found := s.capcompat.Lookup(ctx, key)
	if found {
		if caps.Context == 0 && auto.Context > 0 {
			source = "discovered"
		}
		caps = caps.Merge(auto)
	}

	// Inference is the last resort. It fills only fields still at their zero
	// value after the config and any discovered answer.
	inferred := mc.Inferred
	if found || s.capcompat.Probed(ctx, key) {
		// A capability probe is authoritative for tools once it has run.
		// Tools is a plain bool, so a discovered false cannot override an
		// inferred true through Merge; clear inference explicitly instead.
		inferred.Tools = false
	}
	if caps.Context == 0 && inferred.Context > 0 {
		source = "cmd"
	}
	caps = caps.Merge(inferred)
	return caps, source
}
