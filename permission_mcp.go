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
)

// PermissionMCP is an embedded HTTP MCP server that implements the
// approval_prompt tool Claude Code calls when --permission-prompt-tool
// targets us. Each pending tools/call is parked on a per-RequestID channel
// until the harness's resolve_hook handler delivers a decision; the parked
// HTTP goroutine then writes the JSON-RPC response and unblocks CC.
//
// The transport is HTTP MCP (Streamable HTTP, but we never use streaming —
// every response is a single JSON document). CC posts JSON-RPC requests to
// our /mcp endpoint and we respond synchronously, parking long-running
// tools/call requests on Go channels.
type PermissionMCP struct {
	listener net.Listener
	server   *http.Server
	url      string

	// emit is invoked when CC calls approval_prompt. The harness fires an
	// awaiting_resolution HookEvent and is expected to call Resolve once a
	// decision arrives.
	emit func(toolName string, input json.RawMessage, requestID string)

	mu      sync.Mutex
	pending map[string]chan permissionDecision
	closed  bool
}

// permissionDecision is the resolver's answer to a parked approval_prompt.
type permissionDecision struct {
	Behavior     string          // "allow" | "deny"
	UpdatedInput json.RawMessage // optional; CC will run this instead of the original input
	Message      string          // optional; surfaced alongside denials
}

// NewPermissionMCP starts an HTTP MCP server on a free 127.0.0.1 port.
func NewPermissionMCP(emit func(toolName string, input json.RawMessage, requestID string)) (*PermissionMCP, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	m := &PermissionMCP{
		listener: ln,
		emit:     emit,
		pending:  make(map[string]chan permissionDecision),
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
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
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
				"description": "Bridge permission prompt — defers to bridge-ui or configured resolver",
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

	// Notify the harness so it emits the canonical awaiting_resolution event.
	m.emit(args.ToolName, args.Input, requestID)

	// Block until resolved. Hold indefinitely per design — no timeout.
	// If CC drops the request (interrupt, process exit), drop our parked
	// entry so a late Resolve doesn't deadlock.
	select {
	case d := <-ch:
		writePermissionResult(w, req.ID, d)
	case <-r.Context().Done():
		m.mu.Lock()
		delete(m.pending, requestID)
		m.mu.Unlock()
		// CC already gave up on this request; nothing useful to write.
	}
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
		// crypto/rand failure is system-fatal; the harness will surface it
		// when the parked call never resolves. Fall back to a timestamp-ish
		// value just so we don't return an empty id.
		return "hreq_unseeded"
	}
	return "hreq_" + hex.EncodeToString(b)
}
