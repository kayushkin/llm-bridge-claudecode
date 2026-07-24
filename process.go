package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/ndjson"
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
	err       error         // exit error, set before done is closed

	// otelRecv is the per-process OTLP receiver, if telemetry was enabled
	// at spawn. The exit goroutine shuts it down after CC exits.
	otelRecv *OTelReceiver
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

// ccContentBlock matches Anthropic's content-block wire shape that Claude
// Code accepts on stdin. Exactly one of Text or Source is set, depending on
// Type ("text" → Text; "image"/"document"/"audio"/"video" → Source).
type ccContentBlock struct {
	Type   string         `json:"type"`
	Text   string         `json:"text,omitempty"`
	Source *ccMediaSource `json:"source,omitempty"`
}

// ccMediaSource is the wire shape for image/document/audio/video block sources.
// Type is "base64" (Data + MediaType populated) or "url" (URL populated).
type ccMediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
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
// Permission gating runs as a PreToolUse HTTP hook injected via --settings
// by bridge-server (see internal/server/hook_settings.go). CC's own
// permission system stays off — handleStart hardcodes
// --permission-mode bypassPermissions so CC never consults a
// --permission-prompt-tool we no longer wire.
//
// allowedTools is forwarded as --allowed-tools when non-empty. It just
// narrows the tool surface; the permission gate still fires on every call.
//
// otelRecv, when non-nil, is wired into CC's environment so per-API-call
// telemetry (model, tokens, cost, duration, including auxiliary calls
// invisible to stream-json) flows back as EventAPICall events. The
// returned CCProcess takes ownership and shuts the receiver down after
// the subprocess exits.
func spawnClaudeCode(cfg *Config, sessionID string, allowedTools []string, otelRecv *OTelReceiver, extraArgs ...string) (*CCProcess, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}

	if len(allowedTools) > 0 {
		args = append(args, "--allowed-tools")
		args = append(args, allowedTools...)
	}

	args = append(args, extraArgs...)

	cmd := exec.Command(cfg.ClaudePath, args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}

	env := cmd.Environ()
	if cfg.APIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+cfg.APIKey)
	}
	if otelRecv != nil {
		env = append(env, otelRecv.Env()...)
	}
	cmd.Env = env

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
		otelRecv:  otelRecv,
	}

	// Monitor process exit in background. OTel exporter batches with a 1s
	// interval; give it a short flush window after the subprocess exits
	// before tearing the receiver down so trailing batches land as
	// EventAPICall msg.Events rather than getting dropped.
	go func() {
		p.err = cmd.Wait()
		if p.otelRecv != nil {
			time.Sleep(2 * time.Second)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := p.otelRecv.Stop(shutdownCtx); err != nil {
				log.Printf("[otel] shutdown after process exit: %v", err)
			}
			cancel()
		}
		close(p.done)
	}()

	return p, nil
}

// WriteMessage sends a text-only user message to Claude Code's stdin.
// For multimodal input (images, documents, etc.), use WriteMessageBlocks.
func (p *CCProcess) WriteMessage(content string) error {
	return p.WriteMessageBlocks([]msg.ContentBlock{
		{Type: msg.BlockText, Text: &msg.TextBlock{Text: content}},
	})
}

// WriteMessageBlocks sends a user message composed of canonical content
// blocks (text, image, document, audio, video) to Claude Code's stdin. The
// blocks are translated into Anthropic's content-block wire format that CC
// expects in stream-json input mode.
//
// Only block types valid in user input are accepted; passing tool_use,
// tool_result, thinking, or other model-output kinds returns an error.
// MediaFileID sources are not supported here — callers must resolve them
// to base64 or URL upstream.
func (p *CCProcess) WriteMessageBlocks(blocks []msg.ContentBlock) error {
	wire, err := translateUserContentBlocks(blocks)
	if err != nil {
		return fmt.Errorf("translate user content: %w", err)
	}
	m := ccUserMessage{
		Type: "user",
		Message: ccMessage{
			Role:    "user",
			Content: wire,
		},
		SessionID:       "",
		ParentToolUseID: nil,
	}
	return p.writeJSON(m)
}

// translateUserContentBlocks converts canonical user-input content blocks
// into the wire format Claude Code expects. Returns an error if any block
// is invalid or carries a type that isn't permitted in user input.
func translateUserContentBlocks(blocks []msg.ContentBlock) ([]ccContentBlock, error) {
	out := make([]ccContentBlock, 0, len(blocks))
	for i, blk := range blocks {
		if err := blk.Validate(); err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		switch blk.Type {
		case msg.BlockText:
			out = append(out, ccContentBlock{Type: "text", Text: blk.Text.Text})
		case msg.BlockImage:
			src, err := translateMediaSource(blk.Image.Source)
			if err != nil {
				return nil, fmt.Errorf("block %d (image): %w", i, err)
			}
			out = append(out, ccContentBlock{Type: "image", Source: src})
		case msg.BlockDocument:
			src, err := translateMediaSource(blk.Document.Source)
			if err != nil {
				return nil, fmt.Errorf("block %d (document): %w", i, err)
			}
			out = append(out, ccContentBlock{Type: "document", Source: src})
		case msg.BlockAudio:
			src, err := translateMediaSource(blk.Audio.Source)
			if err != nil {
				return nil, fmt.Errorf("block %d (audio): %w", i, err)
			}
			out = append(out, ccContentBlock{Type: "audio", Source: src})
		case msg.BlockVideo:
			src, err := translateMediaSource(blk.Video.Source)
			if err != nil {
				return nil, fmt.Errorf("block %d (video): %w", i, err)
			}
			out = append(out, ccContentBlock{Type: "video", Source: src})
		default:
			return nil, fmt.Errorf("block %d: type %q not valid in user input", i, blk.Type)
		}
	}
	return out, nil
}

// translateMediaSource converts a canonical msg.MediaSource into Claude
// Code's wire shape. file_id sources are rejected — resolve them to base64
// or URL upstream.
func translateMediaSource(src msg.MediaSource) (*ccMediaSource, error) {
	switch src.Kind {
	case msg.MediaBase64:
		if src.Data == "" {
			return nil, fmt.Errorf("base64 source: empty data")
		}
		return &ccMediaSource{
			Type:      "base64",
			MediaType: src.MediaType,
			Data:      src.Data,
		}, nil
	case msg.MediaURL:
		if src.Data == "" {
			return nil, fmt.Errorf("url source: empty url")
		}
		return &ccMediaSource{
			Type: "url",
			URL:  src.Data,
		}, nil
	case msg.MediaFileID:
		return nil, fmt.Errorf("file_id source not supported by claudecode adapter; resolve to base64 or url upstream")
	default:
		return nil, fmt.Errorf("unknown media source kind: %q", src.Kind)
	}
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
		// ndjson.ReadLine carries no practical line cap and reports an
		// oversized line as its own error, so a single large stream-json
		// event (a big tool result, a multi-MB screenshot) no longer ends
		// the scan indistinguishably from EOF — the old bufio.Scanner
		// failure mode, which closed this channel while Claude Code was
		// still streaming and killed the session mid-turn.
		reader := bufio.NewReader(p.stdout)
		for {
			line, err := ndjson.ReadLine(reader, ndjson.MaxLineBytes)
			if errors.Is(err, ndjson.ErrLineTooLong) {
				log.Printf("dropping stdout event above %d bytes; stream continues", ndjson.MaxLineBytes)
				continue
			}
			if len(line) > 0 {
				raw := make(json.RawMessage, len(line))
				copy(raw, line)
				select {
				case ch <- raw:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("stdout read error: %v", err)
				}
				return
			}
		}
	}()
	return ch
}

// Kill terminates the Claude Code process.
func (p *CCProcess) Kill() error {
	if p.cmd != nil && p.cmd.Process != nil {
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
