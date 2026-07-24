package main

import (
	"bytes"
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
	StringValue *string `json:"stringValue,omitempty"`
	// IntValue is int64. The OTLP/JSON spec encodes it as a quoted string
	// (to avoid JS Number precision loss for >2^53 values), but the Node
	// OTel SDK that ships with Claude Code emits a bare JSON number.
	// otlpInt handles both forms via custom UnmarshalJSON.
	IntValue    *otlpInt `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

// otlpInt accepts either a quoted-string-encoded int64 (OTLP/JSON spec)
// or a bare JSON number (what Node's OTel SDK actually emits). Without
// this, the receiver fails to decode real payloads from Claude Code.
type otlpInt int64

func (o *otlpInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("otlpInt: parse quoted %q: %w", s, err)
		}
		*o = otlpInt(v)
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("otlpInt: parse bare number %q: %w", b, err)
	}
	*o = otlpInt(v)
	return nil
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
//
// Several event types are critical for PTY-mode visibility: claude in
// PTY mode renders the conversation directly to the pty fd, so the only
// way bridge-server learns "the user typed X" or "tool Y was decided
// upon" is via OTel. These translations are no-ops in -p mode (where
// stream-json already carries the same data) but they're the sole
// signal for PTY sessions. Provenance is tagged so consumers can dedupe
// against stream-json equivalents.
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
	case "claude_code.api_error":
		r.emit(translateAPIError(attrs, ts))
	case "claude_code.api_retries_exhausted":
		r.emit(translateAPIRetriesExhausted(attrs, ts))
	case "claude_code.tool_decision":
		r.emit(translateToolDecision(attrs, ts))
	case "claude_code.tool_result":
		r.emit(translateToolResult(attrs, ts))
	case "claude_code.subagent_completed":
		r.emit(translateSubagentCompleted(attrs, ts))
	case "claude_code.user_prompt":
		if ev, ok := translateUserPrompt(attrs, ts); ok {
			r.emit(ev)
		}
	case "claude_code.hook_registered", "claude_code.hook_execution_start", "claude_code.hook_execution_complete":
		// Lifecycle bookkeeping for the PreToolUse / PostToolUse hooks
		// we install via --settings. Already surfaced as bridge HookEvents
		// via the prehook HTTP path; emitting again here would duplicate.
	case "claude_code.assistant_response":
		// A copy of a model text segment. In PTY mode (sidecar) this is the
		// ONLY copy — stream-json doesn't exist — so it flows straight through.
		// In -p mode the authoritative copy also arrives on stream-json, so the
		// harness buffers this tagged copy and only surfaces it if the turn's
		// stream-json produces no assistant text (the drop that stranded the
		// "todo linker" session); see Harness.emit / flushRecoveredAssistant.
		// Empirically the `response` attribute is not truncated (max observed
		// length ~700, no cap cluster), so it's a faithful segment.
		if ev, ok := translateAssistantResponse(attrs, ts); ok {
			r.emit(ev)
		}
	case "claude_code.mcp_server_connection", "claude_code.at_mention", "claude_code.skill_activated", "claude_code.auth":
		// Status/lifecycle signals with no consumer today. Named explicitly so
		// the default branch below stays a real "we've never seen this" alarm
		// rather than routine noise.
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

// translateAPIError maps the OTel `claude_code.api_error` event into an
// EventError. CC emits this for a failed model API call (e.g. a 529
// "Overloaded") that it then retries internally; the failure never appears
// on stream-json, which only carries the eventual result. Surfacing it is
// the difference between a session that visibly stalls on an overloaded API
// and one that looks silently hung — the case this handler was added for.
//
// Retryable mirrors CC's own behavior: it retries on 429 and 5xx, so those
// carry the transient signal. StatusCode is passed through as CC reported it.
func translateAPIError(attrs map[string]string, ts time.Time) msg.Event {
	status := atoiOr(attrs["status_code"], 0)
	ev := msg.Event{
		Type:      msg.EventError,
		Timestamp: ts,
		Error: &msg.ErrorEvent{
			Code:       "api_error",
			Message:    attrs["error"],
			Retryable:  status == 429 || (status >= 500 && status <= 599),
			StatusCode: status,
		},
	}
	tagOTelSource(&ev)
	return ev
}

// translateAPIRetriesExhausted maps the OTel `claude_code.api_retries_exhausted`
// event into an EventError. This is the terminal companion to api_error: CC
// has given up after total_attempts retries and the turn cannot proceed.
// Retryable is false — the retry budget is spent, so this is the failure the
// user needs to see, not a transient blip.
func translateAPIRetriesExhausted(attrs map[string]string, ts time.Time) msg.Event {
	ev := msg.Event{
		Type:      msg.EventError,
		Timestamp: ts,
		Error: &msg.ErrorEvent{
			Code:       "api_retries_exhausted",
			Message:    attrs["error"],
			Retryable:  false,
			StatusCode: atoiOr(attrs["status_code"], 0),
		},
	}
	tagOTelSource(&ev)
	return ev
}

// translateSubagentCompleted maps the OTel `claude_code.subagent_completed`
// event into a SystemEvent. When the main agent spawns a Task-tool subagent
// and waits on it, this is the only signal the bridge gets that the subagent
// finished (the subagent runs inside CC and its stream-json stays private to
// the parent turn). Surfacing it lets a "waiting on N agents" turn show
// progress instead of looking frozen.
func translateSubagentCompleted(attrs map[string]string, ts time.Time) msg.Event {
	ev := msg.Event{
		Type:      msg.EventSystem,
		Timestamp: ts,
		System: &msg.SystemEvent{
			Subtype: "subagent_completed",
			Message: attrs["agent_type"],
		},
	}
	tagOTelSource(&ev)
	return ev
}

// translateAssistantResponse maps the OTel `claude_code.assistant_response`
// event into an assistant text block, tagged source=otel. The consumer decides
// what to do with it: the PTY sidecar forwards it (sole copy of the model's
// text in PTY mode), while the -p harness buffers it and emits it only when
// stream-json delivered no assistant text for the turn — so the healthy path
// never double-renders. Returns ok=false when the body is absent (nothing to
// show).
func translateAssistantResponse(attrs map[string]string, ts time.Time) (msg.Event, bool) {
	body := attrs["response"]
	if body == "" {
		return msg.Event{}, false
	}
	ev := msg.Event{
		Type:      msg.EventBlock,
		Timestamp: ts,
		Block: &msg.BlockEvent{
			Index: 0,
			Block: &msg.ContentBlock{
				Type: msg.BlockText,
				Text: &msg.TextBlock{Text: body},
			},
		},
	}
	tagOTelSource(&ev)
	return ev, true
}

// translateToolDecision maps the OTel `claude_code.tool_decision` event
// (the outcome of claude's PreToolUse permission gate) into a SystemEvent
// with a tool_decision subtype. In -p mode this duplicates information
// already carried by tool_call + the prehook HookEvent; in PTY mode it's
// the only signal a tool was considered.
//
// SystemEvent (not Approval) on purpose — the bridge's EventApproval slot
// is reserved for the prehook's permission *requests*; this is the
// downstream *outcome* the model saw.
func translateToolDecision(attrs map[string]string, ts time.Time) msg.Event {
	ev := msg.Event{
		Type:      msg.EventSystem,
		Timestamp: ts,
		System: &msg.SystemEvent{
			Subtype: "tool_decision",
			Message: attrs["tool_name"] + " → " + attrs["decision"],
		},
	}
	tagOTelSource(&ev)
	return ev
}

// translateToolResult maps the OTel `claude_code.tool_result` event into
// a canonical EventToolResult. OTel carries only metadata (tool name,
// success bool, duration); no args or output bodies, which is why this
// is a useful-but-partial signal for PTY mode. To get the bodies we'd
// need to tail claude's rollout JSONL (deferred follow-up).
//
// ToolID is left empty: OTel doesn't surface claude's internal toolu_*
// id on this event, so we can't pair these results to their tool_calls
// — they render as standalone rows in the chat view. Acceptable for
// PTY-mode visibility; not as nice as the paired -p mode display.
func translateToolResult(attrs map[string]string, ts time.Time) msg.Event {
	success := attrs["success"] == "true"
	ev := msg.Event{
		Type:      msg.EventToolResult,
		Timestamp: ts,
		ToolResult: &msg.ToolResultEvent{
			Name:    attrs["tool_name"],
			IsError: !success,
		},
	}
	tagOTelSource(&ev)
	return ev
}

// translateUserPrompt maps the OTel `claude_code.user_prompt` event into
// an EventUserMessage. The prompt body is only present when
// OTEL_LOG_USER_PROMPTS=1 is set in the env (which the harness/sidecar
// always sets). Returns ok=false when the body is missing — without it
// there's no content to emit and a content-less user_message would
// confuse downstream consumers.
//
// In PTY mode this is the only way bridge-server learns what the user
// typed (the keystrokes go through the pty fd directly to claude, not
// through bridge-server's /send endpoint).
func translateUserPrompt(attrs map[string]string, ts time.Time) (msg.Event, bool) {
	body := attrs["prompt"]
	if body == "" {
		return msg.Event{}, false
	}
	ev := msg.Event{
		Type:      msg.EventUserMessage,
		Timestamp: ts,
		Result: &msg.ResultEvent{
			Text: body,
		},
	}
	tagOTelSource(&ev)
	return ev, true
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
			out[kv.Key] = strconv.FormatInt(int64(*kv.Value.IntValue), 10)
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
