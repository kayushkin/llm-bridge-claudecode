package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
)

// CCProcess manages a single Claude Code subprocess using bidirectional
// stream-json communication (stdin for messages, stdout for events).
type CCProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	mu     sync.Mutex // guards stdin writes

	sessionID string
	done      chan struct{} // closed when process exits
	err       error        // exit error, set before done is closed
}

// ccUserMessage is the JSON format Claude Code expects on stdin for user messages.
type ccUserMessage struct {
	Type            string     `json:"type"`
	Message         ccMessage  `json:"message"`
	SessionID       string     `json:"session_id"`
	ParentToolUseID *string    `json:"parent_tool_use_id"`
}

type ccMessage struct {
	Role    string           `json:"role"`
	Content []ccContentBlock `json:"content"`
}

type ccContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ccControlRequest is the JSON format for control commands sent to CC stdin.
// The request body carries a subtype plus arbitrary additional fields (e.g.
// "model" for set_model, "mode" for set_permission_mode).
type ccControlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

// spawnClaudeCode starts a new Claude Code process with stream-json I/O.
// autoApprove controls permission mode: nil/true uses --dangerously-skip-permissions,
// false uses --allowed-tools with the given tool list.
func spawnClaudeCode(cfg *Config, sessionID string, autoApprove *bool, allowedTools []string, extraArgs ...string) (*CCProcess, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}

	if autoApprove == nil || *autoApprove {
		args = append(args, "--dangerously-skip-permissions")
	} else if len(allowedTools) > 0 {
		args = append(args, "--allowed-tools")
		args = append(args, allowedTools...)
	}

	args = append(args, extraArgs...)

	cmd := exec.Command(cfg.ClaudePath, args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	if cfg.APIKey != "" {
		cmd.Env = append(cmd.Environ(), "ANTHROPIC_API_KEY="+cfg.APIKey)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Forward CC's stderr to our stderr so llm-bridge can capture debug output.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	p := &CCProcess{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		sessionID: sessionID,
		done:      make(chan struct{}),
	}

	// Monitor process exit in background.
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

// WriteMessage sends a user message to Claude Code's stdin.
func (p *CCProcess) WriteMessage(content string) error {
	msg := ccUserMessage{
		Type: "user",
		Message: ccMessage{
			Role: "user",
			Content: []ccContentBlock{
				{Type: "text", Text: content},
			},
		},
		SessionID:       "",
		ParentToolUseID: nil,
	}
	return p.writeJSON(msg)
}

// WriteInterrupt sends an interrupt control_request to Claude Code's stdin.
func (p *CCProcess) WriteInterrupt(requestID string) error {
	return p.WriteControl(requestID, "interrupt", nil)
}

// WriteControl sends a generic control_request to Claude Code's stdin. The
// subtype identifies the command (e.g. "interrupt", "set_model",
// "set_permission_mode"); additional payload fields are merged into the
// request body alongside the subtype.
func (p *CCProcess) WriteControl(requestID, subtype string, payload map[string]any) error {
	body := map[string]any{"subtype": subtype}
	for k, v := range payload {
		if k == "subtype" {
			continue
		}
		body[k] = v
	}
	req := ccControlRequest{
		Type:      "control_request",
		RequestID: requestID,
		Request:   body,
	}
	return p.writeJSON(req)
}

// ReadEvents starts reading stream-json events from Claude Code's stdout.
// Returns a channel that emits raw JSON lines. The channel is closed when
// stdout is exhausted (process exited or stdout closed).
func (p *CCProcess) ReadEvents(ctx context.Context) <-chan json.RawMessage {
	ch := make(chan json.RawMessage, 100)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(p.stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			raw := make(json.RawMessage, len(line))
			copy(raw, line)
			select {
			case ch <- raw:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("stdout scanner error: %v", err)
		}
	}()
	return ch
}

// Kill terminates the Claude Code process.
func (p *CCProcess) Kill() error {
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

// Done returns a channel that is closed when the process exits.
func (p *CCProcess) Done() <-chan struct{} {
	return p.done
}

// Err returns the process exit error (only valid after Done is closed).
func (p *CCProcess) Err() error {
	return p.err
}

// Alive returns true if the process hasn't exited yet.
func (p *CCProcess) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *CCProcess) writeJSON(v any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}
