package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// OTelReceiver is an in-process OTLP/HTTP-JSON listener that the harness
// spawns alongside the Claude Code subprocess. CC exports telemetry to it
// via OTEL_EXPORTER_OTLP_ENDPOINT; we translate the records into
// msg.Events and hand them to the provided emit callback so they flow
// out of the harness on the same NDJSON channel as every other event.
//
// Lifecycle: Start() picks a random localhost port and begins serving;
// EndpointURL() returns the URL CC should be pointed at; Stop(ctx)
// drains and shuts down. Receivers are 1:1 with CC processes so cross-
// session correlation is impossible by construction — the receiver was
// created by the harness that already knows its bridge_session_id.
type OTelReceiver struct {
	emit     func(msg.Event)
	listener net.Listener
	srv      *http.Server
}

// NewOTelReceiver constructs a receiver whose translated events are
// handed to emit. emit must be non-nil and safe for concurrent use.
func NewOTelReceiver(emit func(msg.Event)) (*OTelReceiver, error) {
	if emit == nil {
		return nil, fmt.Errorf("otel receiver: emit callback is required")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("otel receiver listen: %w", err)
	}
	r := &OTelReceiver{emit: emit, listener: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", r.handleLogs)
	mux.HandleFunc("POST /v1/metrics", r.handleMetrics)
	r.srv = &http.Server{Handler: mux}
	return r, nil
}

// Start begins serving in a background goroutine. Returns immediately.
func (r *OTelReceiver) Start() {
	go func() {
		if err := r.srv.Serve(r.listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[otel] receiver serve: %v", err)
		}
	}()
}

// EndpointURL is the value to set on OTEL_EXPORTER_OTLP_ENDPOINT in the
// CC subprocess environment. The HTTP/JSON protocol exports POST to
// {endpoint}/v1/logs and {endpoint}/v1/metrics.
func (r *OTelReceiver) EndpointURL() string {
	addr := r.listener.Addr().(*net.TCPAddr)
	return fmt.Sprintf("http://127.0.0.1:%d", addr.Port)
}

// Env returns the environment variable assignments CC needs to export
// telemetry to this receiver. Appended to cmd.Env by spawnClaudeCode.
func (r *OTelReceiver) Env() []string {
	return []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_LOGS_EXPORTER=otlp",
		"OTEL_METRICS_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + r.EndpointURL(),
		"OTEL_METRIC_EXPORT_INTERVAL=1000",
		"OTEL_LOGS_EXPORT_INTERVAL=1000",
		"OTEL_LOG_USER_PROMPTS=1",
		"OTEL_SERVICE_NAME=llm-bridge-claudecode",
	}
}

// Stop drains pending requests and shuts down. CC's OTel exporter
// batches with a configurable interval; callers should sleep ~2x the
// configured export interval before calling Stop so trailing batches
// land. ctx bounds the graceful-shutdown wait.
func (r *OTelReceiver) Stop(ctx context.Context) error {
	return r.srv.Shutdown(ctx)
}

// --- OTLP/JSON wire types (only the subset we consume) ---

type otlpLogsPayload struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpLogRecord struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Body         otlpAnyValue   `json:"body"`
	Attributes   []otlpKeyValue `json:"attributes"`
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // OTLP/JSON encodes int64 as string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// --- Handlers ---

func (r *OTelReceiver) handleLogs(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var payload otlpLogsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// Surface the parse failure so we don't silently drop telemetry.
		log.Printf("[otel] decode logs payload: %v", err)
		http.Error(w, "decode payload", http.StatusBadRequest)
		return
	}
	for _, rl := range payload.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				r.dispatchLogRecord(lr)
			}
		}
	}
	writeOTLPSuccess(w)
}

func (r *OTelReceiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	// Metrics are roll-ups of the same signals carried in logs (token usage,
	// cost). The harness derives those from the per-call EventAPICall
	// stream, so we drain the request body and acknowledge without parsing.
	// Keeping the endpoint accepting is required — without it, CC's exporter
	// retries indefinitely and prints errors.
	_, _ = io.Copy(io.Discard, req.Body)
	writeOTLPSuccess(w)
}

func writeOTLPSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
}

// dispatchLogRecord routes an OTLP log record to the right translator
// based on the event.name body or attribute. Unknown event names are
// logged for visibility but don't produce a msg.Event — better to skip
// than to fabricate.
func (r *OTelReceiver) dispatchLogRecord(lr otlpLogRecord) {
	name := otlpString(lr.Body)
	attrs := flattenAttrs(lr.Attributes)
	if name == "" {
		name = attrs["event.name"]
	}

	ts := parseUnixNano(lr.TimeUnixNano)

	switch name {
	case "claude_code.api_request":
		r.emit(translateAPIRequest(attrs, ts))
	case "claude_code.internal_error":
		r.emit(translateInternalError(attrs, ts))
	case "claude_code.user_prompt", "claude_code.tool_decision", "claude_code.tool_result":
		// Deferred — stream-json (and the bridge's own prehook for tool_decision)
		// already cover these in -p mode; full TUI-mode synthesis is a separate
		// phase. Log at debug so we know they're flowing without spamming.
	default:
		log.Printf("[otel] unhandled event.name=%q attrs=%v", name, attrs)
	}
}

// --- Translators ---

func translateAPIRequest(attrs map[string]string, ts time.Time) msg.Event {
	ev := msg.Event{
		Type:      msg.EventAPICall,
		Timestamp: ts,
		APICall: &msg.APICallEvent{
			Model:               attrs["model"],
			InputTokens:         atoiOr(attrs["input_tokens"], 0),
			OutputTokens:        atoiOr(attrs["output_tokens"], 0),
			CacheReadTokens:     atoiOr(attrs["cache_read_tokens"], 0),
			CacheCreationTokens: atoiOr(attrs["cache_creation_tokens"], 0),
			CostUSD:             atofOr(attrs["cost_usd"], 0),
			DurationMS:          int64(atoiOr(attrs["duration_ms"], 0)),
			RequestID:           attrs["request_id"],
			QuerySource:         attrs["query_source"],
			Effort:              attrs["effort"],
		},
	}
	tagOTelSource(&ev)
	return ev
}

func translateInternalError(attrs map[string]string, ts time.Time) msg.Event {
	ev := msg.Event{
		Type:      msg.EventError,
		Timestamp: ts,
		Error: &msg.ErrorEvent{
			Code:    "internal_error",
			Message: attrs["message"], // empty when CC doesn't include one — fine; the code is the signal
		},
	}
	tagOTelSource(&ev)
	return ev
}

// tagOTelSource marks the event as OTel-derived so consumers can dedupe
// against the same logical signal arriving via stream-json.
func tagOTelSource(ev *msg.Event) {
	if ev.Extensions == nil {
		ev.Extensions = make(map[string]json.RawMessage, 1)
	}
	ev.Extensions["source"] = json.RawMessage(`"otel"`)
}

// --- Helpers ---

func otlpString(v otlpAnyValue) string {
	if v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

// flattenAttrs collapses an OTLP attribute list into a name→stringified-value
// map. Int and double values are stringified so the caller can parse them
// with atoiOr/atofOr without knowing which variant the producer used.
func flattenAttrs(kvs []otlpKeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		switch {
		case kv.Value.StringValue != nil:
			out[kv.Key] = *kv.Value.StringValue
		case kv.Value.IntValue != nil:
			out[kv.Key] = *kv.Value.IntValue
		case kv.Value.DoubleValue != nil:
			out[kv.Key] = strconv.FormatFloat(*kv.Value.DoubleValue, 'f', -1, 64)
		case kv.Value.BoolValue != nil:
			out[kv.Key] = strconv.FormatBool(*kv.Value.BoolValue)
		}
	}
	return out
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func atofOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

func parseUnixNano(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(0, n)
}
