package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// captureEvents redirects the harness's event sink — emitEvent writes NDJSON
// to os.Stdout — into a pipe, and returns a reader for what was emitted. The
// returned function closes the write end, so call it once.
func captureEvents(t *testing.T) func() []msg.Event {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	restored := false
	t.Cleanup(func() {
		if !restored {
			os.Stdout = prev
			w.Close()
		}
		r.Close()
	})
	return func() []msg.Event {
		os.Stdout = prev
		w.Close()
		restored = true
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read emitted events: %v", err)
		}
		var out []msg.Event
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var e msg.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("emitted line is not an event: %q (%v)", line, err)
			}
			out = append(out, e)
		}
		return out
	}
}

// capturedStdin stands in for the Claude Code process's stdin pipe so a test
// can read back the control requests the harness writes.
type capturedStdin struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedStdin) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedStdin) Close() error { return nil }

func (c *capturedStdin) requests(t *testing.T) []ccControlRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ccControlRequest
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var req ccControlRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("stdin line is not a control request: %q (%v)", line, err)
		}
		out = append(out, req)
	}
	return out
}

// liveHarness builds a harness whose Claude Code process is alive and whose
// stdin is captured, with a process-wide CLAUDE_MODEL default in place.
func liveHarness(defaultModel string) (*Harness, *capturedStdin) {
	stdin := &capturedStdin{}
	return &Harness{
		cfg:  &Config{Model: defaultModel},
		proc: &CCProcess{stdin: stdin, done: make(chan struct{})},
	}, stdin
}

// TestTheServersOwnConfigPayloadSetsTheModel is the regression this file
// exists for. bridge-server sends msg.ConfigSessionRequest verbatim as
// "config:<json>" and that shape has no "subtype" — which the harness used to
// reject outright, so every model change made in the chat UI was silently
// discarded while the API reported success.
func TestTheServersOwnConfigPayloadSetsTheModel(t *testing.T) {
	events := captureEvents(t)

	h, stdin := liveHarness("claude-sonnet-4-5")

	// Byte-for-byte what handleConfigSession marshals for a UI model switch.
	if err := h.handleConfig(json.RawMessage(`{"model":"claude-opus-4-5"}`)); err != nil {
		t.Fatalf("session config rejected: %v", err)
	}

	reqs := stdin.requests(t)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 control request on stdin, got %d", len(reqs))
	}
	if got := reqs[0].Request["subtype"]; got != "set_model" {
		t.Fatalf("control subtype = %v, want set_model", got)
	}
	if got := reqs[0].Request["model"]; got != "claude-opus-4-5" {
		t.Fatalf("control model = %v, want claude-opus-4-5", got)
	}

	var acked bool
	for _, e := range events() {
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "config_updated" {
			acked = true
			if !strings.Contains(e.System.Message, "claude-opus-4-5") {
				t.Fatalf("config_updated does not name the model applied: %q", e.System.Message)
			}
		}
	}
	if !acked {
		t.Fatal("no config_updated system event — the caller cannot tell the change landed")
	}
}

// TestAModelChoiceSurvivesRespawn pins that the model reached by set_model is
// used for the next spawn. --model is a spawn-time flag, so without this a
// resumed session would quietly drop back to the CLAUDE_MODEL default and
// change model mid-conversation.
func TestAModelChoiceSurvivesRespawn(t *testing.T) {
	h, _ := liveHarness("claude-sonnet-4-5")
	if got := h.modelForSpawn(); got != "claude-sonnet-4-5" {
		t.Fatalf("with no session choice, modelForSpawn() = %q, want the CLAUDE_MODEL default", got)
	}

	if err := h.handleConfig(json.RawMessage(`{"model":"claude-opus-4-5"}`)); err != nil {
		t.Fatalf("session config rejected: %v", err)
	}
	if got := h.modelForSpawn(); got != "claude-opus-4-5" {
		t.Fatalf("after set_model, modelForSpawn() = %q, want claude-opus-4-5", got)
	}
}

// TestStartParamsModelBeatsTheEnvDefault covers the other half: a caller that
// names a model per session gets it, and a caller that names none still gets
// the process-wide default. Before this, CLAUDE_MODEL was the only input and
// every session on a harness process ran the same model.
func TestStartParamsModelBeatsTheEnvDefault(t *testing.T) {
	h, _ := liveHarness("claude-sonnet-4-5")
	h.model = ""
	if got := h.modelForSpawn(); got != "claude-sonnet-4-5" {
		t.Fatalf("modelForSpawn() = %q, want the env default", got)
	}

	var params StartParams
	if err := json.Unmarshal([]byte(`{"bridge_session_id":"br_1","model":"claude-haiku-4-5"}`), &params); err != nil {
		t.Fatalf("start params with a model do not parse: %v", err)
	}
	if params.Model != "claude-haiku-4-5" {
		t.Fatalf("StartParams.Model = %q, want claude-haiku-4-5", params.Model)
	}
	h.model = params.Model
	if got := h.modelForSpawn(); got != "claude-haiku-4-5" {
		t.Fatalf("modelForSpawn() = %q, want the session's own model", got)
	}
}

// TestSpawnTimeFlagsAreReportedNotSwallowed covers effort / max_budget /
// disabled_tools: Claude Code takes them as CLI flags at spawn, so a running
// session cannot honour them. They must come back as an error and a system
// event, never as a silent success.
func TestSpawnTimeFlagsAreReportedNotSwallowed(t *testing.T) {
	events := captureEvents(t)

	h, stdin := liveHarness("claude-sonnet-4-5")
	err := h.handleConfig(json.RawMessage(`{"effort":"high","max_budget":5,"disabled_tools":["Bash"]}`))
	if err == nil {
		t.Fatal("changing spawn-time flags mid-session reported success")
	}
	for _, want := range []string{"effort", "max_budget", "disabled_tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	if reqs := stdin.requests(t); len(reqs) != 0 {
		t.Fatalf("expected no control request, got %d", len(reqs))
	}

	var ignored bool
	for _, e := range events() {
		if e.Type == msg.EventSystem && e.System != nil && e.System.Subtype == "config_ignored" {
			ignored = true
		}
	}
	if !ignored {
		t.Fatal("no config_ignored system event — the caller is left assuming the budget applied")
	}
}

// TestAModelChangeSurvivesItsUnappliableSiblings is the mixed payload the chat
// UI actually sends on a new pane: model plus the saved budget default. The
// model must still land.
func TestAModelChangeSurvivesItsUnappliableSiblings(t *testing.T) {
	h, stdin := liveHarness("claude-sonnet-4-5")
	err := h.handleConfig(json.RawMessage(`{"model":"claude-opus-4-5","effort":"high","max_budget":5}`))
	if err == nil {
		t.Fatal("expected the unappliable fields to be reported")
	}
	if strings.Contains(err.Error(), "model") {
		t.Fatalf("model was applied but reported as a failure: %v", err)
	}
	reqs := stdin.requests(t)
	if len(reqs) != 1 || reqs[0].Request["model"] != "claude-opus-4-5" {
		t.Fatalf("the model change did not reach Claude Code: %+v", reqs)
	}
	if h.modelForSpawn() != "claude-opus-4-5" {
		t.Fatalf("model not recorded for respawn: %q", h.modelForSpawn())
	}
}

// TestSubtypeDispatchStillWorks guards the contract this change had to keep:
// payloads that DO name a subtype are Claude Code control requests and must
// still route as before, including unknown subtypes passed straight through.
func TestSubtypeDispatchStillWorks(t *testing.T) {
	h, stdin := liveHarness("")
	if err := h.handleConfig(json.RawMessage(`{"subtype":"set_model","model":"claude-opus-4-5"}`)); err != nil {
		t.Fatalf("subtype set_model rejected: %v", err)
	}
	if err := h.handleConfig(json.RawMessage(`{"subtype":"set_permission_mode","mode":"plan"}`)); err != nil {
		t.Fatalf("unknown subtype pass-through rejected: %v", err)
	}

	reqs := stdin.requests(t)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 control requests, got %d", len(reqs))
	}
	if reqs[0].Request["subtype"] != "set_model" || reqs[0].Request["model"] != "claude-opus-4-5" {
		t.Fatalf("set_model request malformed: %+v", reqs[0].Request)
	}
	if reqs[1].Request["subtype"] != "set_permission_mode" || reqs[1].Request["mode"] != "plan" {
		t.Fatalf("pass-through request malformed: %+v", reqs[1].Request)
	}
}

// TestAnEmptyConfigPayloadIsAnError keeps the fix from turning "the caller
// sent nothing" into a silent no-op — the failure mode the old
// subtype-is-required check was there to catch.
func TestAnEmptyConfigPayloadIsAnError(t *testing.T) {
	h, _ := liveHarness("")
	if err := h.handleConfig(json.RawMessage(`{}`)); err == nil {
		t.Fatal("an empty config payload reported success")
	}
	if err := h.handleConfig(json.RawMessage(`not json`)); err == nil {
		t.Fatal("an unparseable config payload reported success")
	}
}
