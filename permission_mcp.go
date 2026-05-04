package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// PermissionMCP is an embedded HTTP MCP server that implements the
// approval_prompt tool Claude Code calls when --permission-prompt-tool
// targets us. It evaluates each tool call through the supplied Evaluator
// (today: a permission-store HTTP client) and returns one of three outcomes
// to CC: allow / deny / ask. Ask outcomes park the request on a per-RequestID
// channel until the harness's resolve_hook handler delivers a decision; the
// parked HTTP goroutine then writes the JSON-RPC response and unblocks CC.
//
// Bypass mode short-circuits the evaluator entirely — every call returns
// allow without consulting the store. Used as the global "off switch" so a
// broken permission-store doesn't lock up every running session.
type PermissionMCP struct {
	listener net.Listener
	server   *http.Server
	url      string

	// evaluator decides allow / deny / ask for a tool call. Wrapping the
	// permission-store HTTP client in an interface keeps the MCP testable
	// without spinning up the full service stack.
	evaluator Evaluator

	// onAsk fires only when the evaluator returns ask. The harness uses
	// it to emit the canonical phase="awaiting_resolution" HookEvent.
	onAsk AskCallback

	bypassMode atomic.Bool

	mu      sync.Mutex
	pending map[string]chan permissionDecision
	closed  bool
}

// Evaluator returns a verdict for a tool call. Implementations MUST NOT
// block on user input — the ask outcome is what triggers the parked-call
// flow inside PermissionMCP. On any error the implementation should return
// EvaluateResult{Outcome: "ask", Message: <reason>} so the call surfaces to
// a human rather than silently allowing.
type Evaluator func(toolName string, input json.RawMessage) EvaluateResult

// EvaluateResult is the verdict shape independent of how it was reached.
type EvaluateResult struct {
	Outcome       string          // "allow" | "deny" | "ask"
	Message       string          // optional, surfaced on deny / ask
	UpdatedInput  json.RawMessage // optional, on allow: rewrite the tool input
	MatchedRuleID string          // optional, audit/debug
}

// AskCallback is invoked when the evaluator returns ask, immediately before
// the MCP parks the call. It carries the tool name + input + the parked
// request id so the caller (the harness) can emit the matching
// awaiting_resolution HookEvent.
type AskCallback func(toolName string, input json.RawMessage, requestID string)

// permissionDecision is the resolver's answer to a parked approval_prompt.
type permissionDecision struct {
	Behavior     string          // "allow" | "deny"
	UpdatedInput json.RawMessage // optional; CC will run this instead of the original input
	Message      string          // optional; surfaced alongside denials
}

// NewPermissionMCP starts an HTTP MCP server on a free 127.0.0.1 port.
func NewPermissionMCP(evaluator Evaluator, onAsk AskCallback) (*PermissionMCP, error) {
	if evaluator == nil {
		return nil, fmt.Errorf("permission MCP requires an evaluator")
	}
	if onAsk == nil {
		return nil, fmt.Errorf("permission MCP requires an onAsk callback")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	m := &PermissionMCP{
		listener:  ln,
		evaluator: evaluator,
		onAsk:     onAsk,
		pending:   make(map[string]chan permissionDecision),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.handle)
	m.server = &http.Server{Handler: mux}
	m.url = fmt.Sprintf("http://%s/mcp", ln.Addr().String())
	go func() {
		if err := m.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[permission-mcp] serve: %v", err)
		}
	}()
	return m, nil
}

// URL returns the MCP endpoint URL CC should connect to.
func (m *PermissionMCP) URL() string { return m.url }

// SetBypass flips the global bypass flag. When true, every tool call
// resolves to allow without consulting the evaluator. Live sessions feel
// the change on their next tool call (no respawn needed).
func (m *PermissionMCP) SetBypass(enabled bool) {
	m.bypassMode.Store(enabled)
}

// Bypass reports the current bypass-mode setting.
func (m *PermissionMCP) Bypass() bool {
	return m.bypassMode.Load()
}

// supportedProtocolVersions lists every MCP revision our minimal server
// is wire-compatible with. We only implement initialize, tools/list,
// tools/call, notifications/initialized, and notifications/cancelled —
// none of those JSON-RPC shapes have changed across the listed revisions.
//
// Order is newest-first so negotiateProtocolVersion picks the latest
// mutually-supported version when the client asks for something we
// recognize.
//
// Maintenance: if a future MCP revision changes the shape of any
// endpoint we implement, drop the affected revisions here. Don't add
// a revision to this list speculatively — the contract is "we promise
// to behave correctly under this revision".
var supportedProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
}

// negotiateProtocolVersion implements MCP's version handshake: prefer the
// client's requested version when we support it, otherwise return our
// latest. Per the MCP spec the client then decides whether to proceed
// with the version we returned (it must terminate if it doesn't support
// what we sent — that's a clean failure, not a silent mismatch).
func negotiateProtocolVersion(clientVersion string) string {
	for _, v := range supportedProtocolVersions {
		if v == clientVersion {
			return v
		}
	}
	return supportedProtocolVersions[0]
}

// ServerName is the MCP server name CC sees in --mcp-config and the
// --permission-prompt-tool flag (mcp__<server>__<tool>).
const PermissionMCPServerName = "bridge_perm"

// PermissionMCPToolName is the single tool the server exposes.
const PermissionMCPToolName = "approval_prompt"

// PermissionPromptToolSpec returns the value to pass to
// --permission-prompt-tool: mcp__<server>__<tool>.
func PermissionPromptToolSpec() string {
	return fmt.Sprintf("mcp__%s__%s", PermissionMCPServerName, PermissionMCPToolName)
}

// MCPConfigJSON returns a --mcp-config payload registering the embedded
// server under PermissionMCPServerName.
func (m *PermissionMCP) MCPConfigJSON() ([]byte, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			PermissionMCPServerName: map[string]any{
				"type": "http",
				"url":  m.url,
			},
		},
	}
	return json.Marshal(cfg)
}

// Resolve delivers a decision for a parked RequestID. Returns false when the
// RequestID is unknown (already resolved, never existed, or the connection
// was dropped before resolution).
func (m *PermissionMCP) Resolve(requestID string, d permissionDecision) bool {
	m.mu.Lock()
	ch, ok := m.pending[requestID]
	if ok {
		delete(m.pending, requestID)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	ch <- d
	close(ch)
	return true
}

// PendingIDs returns the RequestIDs currently parked. Used on harness
// shutdown / fork so the caller can deny-fail anything outstanding.
func (m *PermissionMCP) PendingIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	return ids
}

// Shutdown closes the HTTP server. Any parked approval_prompt calls are
// resolved as deny:"harness shutting down" so CC doesn't hang.
func (m *PermissionMCP) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	for id, ch := range m.pending {
		ch <- permissionDecision{Behavior: "deny", Message: "harness shutting down"}
		close(ch)
		delete(m.pending, id)
	}
	m.mu.Unlock()
	return m.server.Shutdown(ctx)
}

// --- HTTP / JSON-RPC plumbing ---

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (m *PermissionMCP) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error")
		return
	}
	switch req.Method {
	case "initialize":
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &initParams)
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": negotiateProtocolVersion(initParams.ProtocolVersion),
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "llm-bridge-permission",
				"version": version,
			},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{{
				"name":        PermissionMCPToolName,
				"description": "Bridge permission prompt — defers to permission-store rules and bridge-ui",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool_name": map[string]any{"type": "string"},
						"input":     map[string]any{"type": "object"},
					},
					"required": []string{"tool_name", "input"},
				},
			}},
		})
	case "tools/call":
		m.handleToolCall(w, r, req)
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (m *PermissionMCP) handleToolCall(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	if p.Name != PermissionMCPToolName {
		writeJSONRPCError(w, req.ID, -32602, "unknown tool: "+p.Name)
		return
	}
	var args struct {
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(p.Arguments, &args); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid arguments: "+err.Error())
		return
	}

	// Bypass short-circuit — happens before evaluator so a broken
	// permission-store can't lock up every session.
	if m.bypassMode.Load() {
		writePermissionResult(w, req.ID, permissionDecision{Behavior: "allow"})
		return
	}

	result := m.evaluator(args.ToolName, args.Input)

	switch result.Outcome {
	case "allow":
		writePermissionResult(w, req.ID, permissionDecision{
			Behavior:     "allow",
			UpdatedInput: result.UpdatedInput,
		})
		return
	case "deny":
		writePermissionResult(w, req.ID, permissionDecision{
			Behavior: "deny",
			Message:  result.Message,
		})
		return
	}

	// "ask" — park the call until the harness's resolve_hook delivers
	// a decision. Two transport-layer hazards make this non-trivial:
	//   1. CC's HTTP MCP transport closes the request body after ~60s of
	//      silence even when MCP_TOOL_TIMEOUT permits a longer wait.
	//   2. Any keepalive bytes have to ride a transport CC accepts.
	// Per the MCP Streamable HTTP spec, a tools/call response can be
	// either application/json (single payload) or text/event-stream (a
	// stream that may carry progress notifications followed by the final
	// result). Switching to SSE here lets us emit a JSON-RPC progress
	// notification every ~25s as a keepalive, which both keeps the TCP
	// connection from going idle and resets CC's onprogress watchdog
	// (which arms `armedAt` on transport silence and aborts at 90s).
	requestID := newRequestID()
	ch := make(chan permissionDecision, 1)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		writeJSONRPCError(w, req.ID, -32000, "permission server shutting down")
		return
	}
	m.pending[requestID] = ch
	m.mu.Unlock()

	m.onAsk(args.ToolName, args.Input, requestID)

	// Pull the progressToken out of the request _meta if the client sent
	// one; including it on our progress notifications is what makes them
	// route to onprogress for *this* parked tools/call.
	progressToken := extractProgressToken(req.Params)

	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		// http.ResponseWriter must implement Flusher for SSE. Go's stdlib
		// always does, but if a wrapper strips it we fall back to the
		// blocking JSON path; CC will still time out at 60s on this path,
		// but at least the call won't deadlock waiting for a flush that
		// never happens.
		select {
		case d := <-ch:
			writePermissionResult(w, req.ID, d)
		case <-r.Context().Done():
			m.mu.Lock()
			delete(m.pending, requestID)
			m.mu.Unlock()
		}
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // hint to disable any reverse-proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case d := <-ch:
			sseSendResult(w, flusher, req.ID, d)
			return
		case <-keepalive.C:
			sseSendKeepalive(w, flusher, progressToken)
		case <-r.Context().Done():
			m.mu.Lock()
			delete(m.pending, requestID)
			m.mu.Unlock()
			// CC closed the request — nothing useful to write.
			return
		}
	}
}

// extractProgressToken pulls _meta.progressToken from a JSON-RPC request's
// params, returning "" if absent. Per MCP spec, clients that want to
// receive progress notifications must include this token; including it on
// our notifications is how the client routes them back to the right call.
func extractProgressToken(params json.RawMessage) string {
	var p struct {
		Meta struct {
			ProgressToken any `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	switch v := p.Meta.ProgressToken.(type) {
	case string:
		return v
	case float64:
		// JSON numbers decode as float64; re-encode as int-like string.
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// sseSendResult writes the final tools/call response as one SSE event and
// closes the stream by returning to handleToolCall's caller (which lets
// the response writer finalize). The wire format follows the MCP
// Streamable HTTP "message" event shape.
func sseSendResult(w http.ResponseWriter, flusher http.Flusher, id json.RawMessage, d permissionDecision) {
	payload := map[string]any{"behavior": d.Behavior}
	if len(d.UpdatedInput) > 0 {
		payload["updatedInput"] = json.RawMessage(d.UpdatedInput)
	}
	if d.Message != "" {
		payload["message"] = d.Message
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[permission-mcp] sse marshal decision: %v", err)
		return
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": string(body),
			}},
		},
	}
	if id != nil {
		resp["id"] = json.RawMessage(id)
	}
	respJSON, err := json.Marshal(resp)
	if err != nil {
		log.Printf("[permission-mcp] sse marshal envelope: %v", err)
		return
	}
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", respJSON)
	flusher.Flush()
}

// sseSendKeepalive emits a JSON-RPC progress notification (with the
// client-supplied progressToken when present) so CC routes it through
// onprogress and resets its transport watchdog. With no token we send an
// SSE comment, which is harmless and keeps the TCP path warm but does
// not reset onprogress.
func sseSendKeepalive(w http.ResponseWriter, flusher http.Flusher, progressToken string) {
	if progressToken == "" {
		fmt.Fprint(w, ": waiting on resolver\n\n")
		flusher.Flush()
		return
	}
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": progressToken,
			"progress":      0,
			"message":       "waiting on human approval",
		},
	}
	body, err := json.Marshal(notif)
	if err != nil {
		log.Printf("[permission-mcp] sse marshal keepalive: %v", err)
		return
	}
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
	flusher.Flush()
}

// writePermissionResult writes the MCP tool/call response in the shape
// CC's --permission-prompt-tool expects: a single text content block whose
// text is a JSON document with {behavior, updatedInput?, message?}.
func writePermissionResult(w http.ResponseWriter, id json.RawMessage, d permissionDecision) {
	payload := map[string]any{"behavior": d.Behavior}
	if len(d.UpdatedInput) > 0 {
		payload["updatedInput"] = json.RawMessage(d.UpdatedInput)
	}
	if d.Message != "" {
		payload["message"] = d.Message
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeJSONRPCError(w, id, -32603, "marshal decision: "+err.Error())
		return
	}
	writeJSONRPCResult(w, id, map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(body),
		}},
	})
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := map[string]any{"jsonrpc": "2.0", "result": result}
	if id != nil {
		resp["id"] = json.RawMessage(id)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[permission-mcp] write response: %v", err)
	}
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": message},
	}
	if id != nil {
		resp["id"] = json.RawMessage(id)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[permission-mcp] write error: %v", err)
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "hreq_unseeded"
	}
	return "hreq_" + hex.EncodeToString(b)
}
