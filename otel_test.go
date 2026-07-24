package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestOTelReceiver_APIRequestTranslation exercises the receiver end-to-end
// via HTTP: it constructs the same OTLP/JSON envelope CC emits for a single
// `claude_code.api_request` log record, POSTs it to the receiver, and
// verifies the emit callback got a well-formed EventAPICall with the
// `source=otel` provenance tag.
func TestOTelReceiver_APIRequestTranslation(t *testing.T) {
	var (
		mu     sync.Mutex
		events []msg.Event
	)
	emit := func(e msg.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}

	recv, err := NewOTelReceiver(emit)
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	// Mix bare-number and quoted-string intValue forms in the same payload
	// — the Node OTel SDK (what Claude Code ships) emits bare numbers,
	// while OTLP/JSON spec defines quoted strings. Both must decode.
	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1778780614432000000",
					"body": {"stringValue": "claude_code.api_request"},
					"attributes": [
						{"key": "event.name",           "value": {"stringValue": "api_request"}},
						{"key": "event.sequence",      "value": {"intValue": 2}},
						{"key": "model",                "value": {"stringValue": "claude-opus-4-7"}},
						{"key": "input_tokens",        "value": {"intValue": 6}},
						{"key": "output_tokens",       "value": {"intValue": 6}},
						{"key": "cache_read_tokens",   "value": {"intValue": 17913}},
						{"key": "cache_creation_tokens","value": {"intValue": "9268"}},
						{"key": "cost_usd",             "value": {"doubleValue": 0.0670615}},
						{"key": "duration_ms",          "value": {"intValue": "2829"}},
						{"key": "request_id",           "value": {"stringValue": "req_011Cb2sjmBKCyjSmxQ8Byyor"}},
						{"key": "query_source",         "value": {"stringValue": "sdk"}},
						{"key": "effort",               "value": {"stringValue": "xhigh"}}
					]
				}]
			}]
		}]
	}`

	resp, err := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Translation runs synchronously in the handler — by the time POST
	// returned, emit has been called. No sleep needed.
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]

	if ev.Type != msg.EventAPICall {
		t.Errorf("type: want %q, got %q", msg.EventAPICall, ev.Type)
	}
	if ev.APICall == nil {
		t.Fatal("APICall payload is nil")
	}
	if ev.APICall.Model != "claude-opus-4-7" {
		t.Errorf("model: %q", ev.APICall.Model)
	}
	if ev.APICall.InputTokens != 6 || ev.APICall.OutputTokens != 6 {
		t.Errorf("tokens: in=%d out=%d", ev.APICall.InputTokens, ev.APICall.OutputTokens)
	}
	if ev.APICall.CacheReadTokens != 17913 {
		t.Errorf("cache_read: %d", ev.APICall.CacheReadTokens)
	}
	if ev.APICall.CacheCreationTokens != 9268 {
		t.Errorf("cache_creation: %d", ev.APICall.CacheCreationTokens)
	}
	if ev.APICall.CostUSD != 0.0670615 {
		t.Errorf("cost: %v", ev.APICall.CostUSD)
	}
	if ev.APICall.DurationMS != 2829 {
		t.Errorf("duration: %d", ev.APICall.DurationMS)
	}
	if ev.APICall.RequestID != "req_011Cb2sjmBKCyjSmxQ8Byyor" {
		t.Errorf("request_id: %q", ev.APICall.RequestID)
	}
	if ev.APICall.QuerySource != "sdk" {
		t.Errorf("query_source: %q", ev.APICall.QuerySource)
	}
	if ev.APICall.Effort != "xhigh" {
		t.Errorf("effort: %q", ev.APICall.Effort)
	}

	// Provenance tag — consumers dedupe against the same logical signal
	// arriving via stream-json by inspecting this.
	src, ok := ev.Extensions["source"]
	if !ok {
		t.Fatal("missing Extensions[source]")
	}
	if string(src) != `"otel"` {
		t.Errorf("source tag: %q", string(src))
	}

	// Timestamp is parsed from the OTLP timeUnixNano, not "now".
	expected := time.Unix(0, 1778780614432000000)
	if !ev.Timestamp.Equal(expected) {
		t.Errorf("timestamp: want %v, got %v", expected, ev.Timestamp)
	}
}

// TestOTelReceiver_InternalErrorTranslation verifies that the
// `claude_code.internal_error` log record maps to EventError. CC emits
// these on tool failures even though the surrounding turn result reports
// `success` — this is the only typed-error signal we get.
func TestOTelReceiver_InternalErrorTranslation(t *testing.T) {
	var got msg.Event
	var gotOnce sync.Once

	recv, err := NewOTelReceiver(func(e msg.Event) {
		gotOnce.Do(func() { got = e })
	})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1778782946151000000",
					"body": {"stringValue": "claude_code.internal_error"},
					"attributes": [
						{"key": "event.name", "value": {"stringValue": "internal_error"}},
						{"key": "message",    "value": {"stringValue": "Read of nonexistent path failed"}}
					]
				}]
			}]
		}]
	}`
	resp, _ := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	resp.Body.Close()

	if got.Type != msg.EventError {
		t.Fatalf("type: want %q, got %q", msg.EventError, got.Type)
	}
	if got.Error == nil {
		t.Fatal("Error payload is nil")
	}
	if got.Error.Code != "internal_error" {
		t.Errorf("code: %q", got.Error.Code)
	}
	if got.Error.Message != "Read of nonexistent path failed" {
		t.Errorf("message: %q", got.Error.Message)
	}
}

// TestOTelReceiver_APIErrorAndRetriesExhausted confirms the two API-failure
// events CC emits over OTel (and never over stream-json, because it retries
// internally) surface as typed EventError instead of being silently dropped.
// This is the gap that made overloaded-API sessions look hung with no error
// anywhere.
func TestOTelReceiver_APIErrorAndRetriesExhausted(t *testing.T) {
	var (
		mu     sync.Mutex
		events []msg.Event
	)
	recv, err := NewOTelReceiver(func(e msg.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [
					{
						"body": {"stringValue": "claude_code.api_error"},
						"attributes": [
							{"key": "event.name",  "value": {"stringValue": "api_error"}},
							{"key": "error",        "value": {"stringValue": "Overloaded"}},
							{"key": "status_code",  "value": {"stringValue": "529"}},
							{"key": "attempt",      "value": {"stringValue": "11"}}
						]
					},
					{
						"body": {"stringValue": "claude_code.api_retries_exhausted"},
						"attributes": [
							{"key": "event.name",    "value": {"stringValue": "api_retries_exhausted"}},
							{"key": "error",          "value": {"stringValue": "Overloaded"}},
							{"key": "status_code",    "value": {"stringValue": "529"}},
							{"key": "total_attempts", "value": {"stringValue": "11"}}
						]
					}
				]
			}]
		}]
	}`
	resp, err := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	apiErr := events[0]
	if apiErr.Type != msg.EventError || apiErr.Error == nil ||
		apiErr.Error.Code != "api_error" || apiErr.Error.Message != "Overloaded" ||
		apiErr.Error.StatusCode != 529 || !apiErr.Error.Retryable {
		t.Errorf("api_error mapped wrong: %+v / %+v", apiErr.Type, apiErr.Error)
	}

	exhausted := events[1]
	if exhausted.Type != msg.EventError || exhausted.Error == nil ||
		exhausted.Error.Code != "api_retries_exhausted" || exhausted.Error.StatusCode != 529 ||
		exhausted.Error.Retryable {
		t.Errorf("api_retries_exhausted mapped wrong: %+v / %+v", exhausted.Type, exhausted.Error)
	}

	for i, ev := range events {
		if string(ev.Extensions["source"]) != `"otel"` {
			t.Errorf("event[%d] missing otel source tag: %v", i, ev.Extensions)
		}
	}
}

// TestOTelReceiver_SubagentCompleted confirms a finished Task-tool subagent
// surfaces as a system event so a waiting parent turn shows progress.
func TestOTelReceiver_SubagentCompleted(t *testing.T) {
	var got msg.Event
	var gotOnce sync.Once
	recv, err := NewOTelReceiver(func(e msg.Event) { gotOnce.Do(func() { got = e }) })
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"body": {"stringValue": "claude_code.subagent_completed"},
					"attributes": [
						{"key": "event.name", "value": {"stringValue": "subagent_completed"}},
						{"key": "agent_type", "value": {"stringValue": "custom"}}
					]
				}]
			}]
		}]
	}`
	resp, _ := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	resp.Body.Close()

	if got.Type != msg.EventSystem || got.System == nil ||
		got.System.Subtype != "subagent_completed" || got.System.Message != "custom" {
		t.Fatalf("subagent_completed mapped wrong: %+v / %+v", got.Type, got.System)
	}
}

// TestOTelReceiver_AssistantResponseTaggedBlock confirms assistant_response
// maps to a source=otel text block. The receiver always emits it; the consumer
// (PTY sidecar vs -p harness) decides whether to forward or buffer-and-recover.
// A body-less event emits nothing.
func TestOTelReceiver_AssistantResponseTaggedBlock(t *testing.T) {
	var (
		mu     sync.Mutex
		events []msg.Event
	)
	recv, err := NewOTelReceiver(func(e msg.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [
					{
						"body": {"stringValue": "claude_code.assistant_response"},
						"attributes": [
							{"key": "event.name",      "value": {"stringValue": "assistant_response"}},
							{"key": "response",        "value": {"stringValue": "Want me to (a) write the doc?"}},
							{"key": "response_length", "value": {"stringValue": "29"}}
						]
					},
					{
						"body": {"stringValue": "claude_code.assistant_response"},
						"attributes": [
							{"key": "event.name",      "value": {"stringValue": "assistant_response"}},
							{"key": "response_length", "value": {"stringValue": "0"}}
						]
					}
				]
			}]
		}]
	}`
	resp, _ := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected 1 event (body-less one skipped), got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventBlock || e.Block == nil || e.Block.Block == nil ||
		e.Block.Block.Type != msg.BlockText || e.Block.Block.Text == nil ||
		e.Block.Block.Text.Text != "Want me to (a) write the doc?" {
		t.Fatalf("assistant_response block malformed: %+v", e)
	}
	if string(e.Extensions["source"]) != `"otel"` {
		t.Errorf("missing otel source tag: %v", e.Extensions)
	}
}

// TestOTelReceiver_MetricsAccept confirms /v1/metrics returns 200 even
// though we don't translate metrics. Without this, CC's OTLP exporter
// retries indefinitely and floods stderr.
func TestOTelReceiver_MetricsAccept(t *testing.T) {
	recv, err := NewOTelReceiver(func(msg.Event) {})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	resp, err := http.Post(recv.EndpointURL()+"/v1/metrics", "application/json",
		bytes.NewBufferString(`{"resourceMetrics":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("metrics status: %d", resp.StatusCode)
	}
}

// TestOTelReceiver_EnvIncludesEndpoint sanity-checks the env-var bundle —
// CC won't export telemetry unless these exact variables are set.
func TestOTelReceiver_EnvIncludesEndpoint(t *testing.T) {
	recv, err := NewOTelReceiver(func(msg.Event) {})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	defer recv.Stop(context.Background())

	env := recv.Env()
	want := map[string]bool{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1":                    false,
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json":             false,
		"OTEL_LOGS_EXPORTER=otlp":                           false,
		"OTEL_METRICS_EXPORTER=otlp":                        false,
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + recv.EndpointURL(): false,
	}
	for _, kv := range env {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for k, present := range want {
		if !present {
			t.Errorf("missing env: %s", k)
		}
	}
}

// drainAll is a small helper not strictly used in the tests above but
// useful when promoting these to an e2e flow that spawns the binary.
var _ = json.Marshal

func TestOTelReceiver_ToolDecisionAndResult(t *testing.T) {
	var (
		mu     sync.Mutex
		events []msg.Event
	)
	emit := func(e msg.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	recv, err := NewOTelReceiver(emit)
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [
					{
						"timeUnixNano": "1778796549000000000",
						"body": {"stringValue": "claude_code.tool_decision"},
						"attributes": [
							{"key": "event.name", "value": {"stringValue": "tool_decision"}},
							{"key": "tool_name",  "value": {"stringValue": "Bash"}},
							{"key": "decision",   "value": {"stringValue": "accept"}}
						]
					},
					{
						"timeUnixNano": "1778796549050000000",
						"body": {"stringValue": "claude_code.tool_result"},
						"attributes": [
							{"key": "event.name",  "value": {"stringValue": "tool_result"}},
							{"key": "tool_name",   "value": {"stringValue": "Bash"}},
							{"key": "success",     "value": {"stringValue": "true"}}
						]
					},
					{
						"timeUnixNano": "1778796549100000000",
						"body": {"stringValue": "claude_code.tool_result"},
						"attributes": [
							{"key": "event.name", "value": {"stringValue": "tool_result"}},
							{"key": "tool_name",  "value": {"stringValue": "Read"}},
							{"key": "success",    "value": {"stringValue": "false"}}
						]
					}
				]
			}]
		}]
	}`
	resp, err := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	dec := events[0]
	if dec.Type != msg.EventSystem || dec.System == nil ||
		dec.System.Subtype != "tool_decision" || dec.System.Message != "Bash → accept" {
		t.Errorf("tool_decision: %+v / %+v", dec.Type, dec.System)
	}

	ok := events[1]
	if ok.Type != msg.EventToolResult || ok.ToolResult == nil ||
		ok.ToolResult.Name != "Bash" || ok.ToolResult.IsError {
		t.Errorf("tool_result success: %+v / %+v", ok.Type, ok.ToolResult)
	}

	bad := events[2]
	if bad.Type != msg.EventToolResult || bad.ToolResult == nil ||
		bad.ToolResult.Name != "Read" || !bad.ToolResult.IsError {
		t.Errorf("tool_result failure: %+v / %+v", bad.Type, bad.ToolResult)
	}

	for i, ev := range events {
		if string(ev.Extensions["source"]) != `"otel"` {
			t.Errorf("event[%d] missing otel source tag: %v", i, ev.Extensions)
		}
	}
}

func TestOTelReceiver_UserPrompt(t *testing.T) {
	var got msg.Event
	var skipped int
	var mu sync.Mutex
	emit := func(e msg.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = e
	}
	recv, err := NewOTelReceiver(emit)
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv.Start()
	defer recv.Stop(context.Background())

	// With prompt body present → emits a user_message.
	with := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1778796549000000000",
					"body": {"stringValue": "claude_code.user_prompt"},
					"attributes": [
						{"key": "event.name",    "value": {"stringValue": "user_prompt"}},
						{"key": "prompt_length", "value": {"intValue": 5}},
						{"key": "prompt",        "value": {"stringValue": "hello"}}
					]
				}]
			}]
		}]
	}`
	resp, _ := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(with))
	resp.Body.Close()

	mu.Lock()
	if got.Type != msg.EventUserMessage || got.Result == nil || got.Result.Text != "hello" {
		t.Fatalf("user_prompt with body: %+v", got)
	}
	mu.Unlock()

	// Without prompt body → no emission (count via a fresh receiver).
	recv2, err := NewOTelReceiver(func(msg.Event) {
		mu.Lock()
		skipped++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}
	recv2.Start()
	defer recv2.Stop(context.Background())

	without := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1778796549000000000",
					"body": {"stringValue": "claude_code.user_prompt"},
					"attributes": [
						{"key": "event.name",    "value": {"stringValue": "user_prompt"}},
						{"key": "prompt_length", "value": {"intValue": 5}}
					]
				}]
			}]
		}]
	}`
	resp2, _ := http.Post(recv2.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(without))
	resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if skipped != 0 {
		t.Errorf("user_prompt without body should be skipped, got %d emissions", skipped)
	}
}
