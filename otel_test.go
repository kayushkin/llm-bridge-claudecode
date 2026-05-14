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
		"CLAUDE_CODE_ENABLE_TELEMETRY=1":              false,
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json":       false,
		"OTEL_LOGS_EXPORTER=otlp":                     false,
		"OTEL_METRICS_EXPORTER=otlp":                  false,
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
