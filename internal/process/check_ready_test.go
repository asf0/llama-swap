package process

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"gopkg.in/yaml.v3"
)

// sequenceUpstream serves each body in bodies in order, repeating the last one
// once the sequence is exhausted. Every response is HTTP 200 so the tests
// exercise the readiness predicate rather than the status code.
func sequenceUpstream(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(polls.Add(1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, bodies[n])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCheckReadyProcess(t *testing.T, proxy string, cr *config.CheckReadyConfig) *ProcessCommand {
	t.Helper()
	skipIfNoSimpleResponder(t)
	cmd, _ := simpleResponderCmd(t, "-silent")
	return newProcessCommand(t, config.ModelConfig{
		Cmd:           cmd,
		Proxy:         proxy,
		CheckEndpoint: "/health",
		CheckReady:    cr,
	})
}

func TestProcessCommand_CheckReadyJSONPath(t *testing.T) {
	mock := sequenceUpstream(t,
		`{"model":null,"progress":0.25,"message":"loading weights"}`,
		`{"model":"qwen3.8:flash-next"}`,
	)
	p := newCheckReadyProcess(t, mock.URL, &config.CheckReadyConfig{JSONPath: "$.model"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.EnsureReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer p.Stop(testStopTimeout)

	if got := p.State(); got != StateReady {
		t.Errorf("state = %q, want ready", got)
	}
}

func TestProcessCommand_CheckReadyBodyRegex(t *testing.T) {
	mock := sequenceUpstream(t,
		`{"loading_model":null}`,
		`{"model":"qwen3.8:flash-next"}`,
	)
	cr := &config.CheckReadyConfig{}
	// BodyRegex is normally compiled by UnmarshalYAML; compile through the same
	// path so the test covers the real configuration shape.
	if err := yaml.Unmarshal([]byte(`bodyRegex: '"loading_model"\s*:\s*null'`), cr); err != nil {
		t.Fatalf("compile bodyRegex: %v", err)
	}
	p := newCheckReadyProcess(t, mock.URL, cr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.EnsureReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer p.Stop(testStopTimeout)

	if got := p.State(); got != StateReady {
		t.Errorf("state = %q, want ready", got)
	}
}

func TestProcessCommand_CheckReadyStillLoadingTimesOut(t *testing.T) {
	mock := sequenceUpstream(t, `{"model":null}`)
	p := newCheckReadyProcess(t, mock.URL, &config.CheckReadyConfig{JSONPath: "$.model"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := p.EnsureReady(ctx, 1500*time.Millisecond)
	if err == nil || !strings.HasSuffix(err.Error(), "health check timed out after 1.5s") {
		t.Fatalf("EnsureReady error = %v, want a health check timeout", err)
	}
}
