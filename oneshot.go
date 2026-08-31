package main

// -oneshot mode: a stateless, single-turn structured LLM call that runs on
// the operator's Claude Code subscription login.
//
// Contract (see llm-bridge-server/internal/server/oneshot.go): read a
// msg.OneShotRequest JSON from stdin, write a msg.OneShotResponse JSON to
// stdout, exit 0. On failure, write {"error":"..."} to stdout and exit 1 so
// the server can surface the message verbatim.
//
// The call is made by spawning `claude -p --output-format json` and letting
// the CLI authenticate from its own login (~/.claude/.credentials.json).
// That is the whole point of this mode: batch classifiers that used to POST
// to api.anthropic.com with an API key route here instead and bill the
// subscription. Because credential choice is the point, ambiguity about
// which credential would be used is a hard error, never a silent pick:
//
//   - LLMBRIDGE_CREDENTIAL_ID set (the instance has a bound credential):
//     refused. This mode does not resolve auth-store credentials; run such
//     instances through a session instead.
//   - ANTHROPIC_API_KEY present in the environment: refused. The spawned CLI
//     would bill the API key while the caller believes it is on the
//     subscription.
//
// Schema conformance uses the CLI's own --json-schema flag, which is a
// forced tool call underneath (stop_reason reports "tool_use"), satisfying
// msg.OneShotRequest's hard-force requirement natively. The reply is still
// validated as exactly one JSON object here: a reply truncated at the
// output-token cap fails that validation loudly instead of parsing as a
// shorter valid answer, and stop_reason is passed through so callers can
// keep their max_tokens guards.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// oneShotTimeout bounds the spawned CLI. The server wraps the whole exec in
// its own 6-minute context; this sits just under it so the timeout error the
// caller sees names the real culprit (the model call) rather than a generic
// killed process.
const oneShotTimeout = 5 * time.Minute

func runOneShot() int {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeOneShotError("read stdin: " + err.Error())
		return 1
	}
	var req msg.OneShotRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOneShotError("decode request: " + err.Error())
		return 1
	}
	if req.Prompt == "" {
		writeOneShotError("prompt required")
		return 1
	}
	if id := os.Getenv("LLMBRIDGE_CREDENTIAL_ID"); id != "" {
		writeOneShotError("instance has a bound credential (" + id + "); oneshot mode runs on the Claude Code login and does not resolve auth-store credentials — use a session instead")
		return 1
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		writeOneShotError("ANTHROPIC_API_KEY is set in the harness environment; oneshot mode exists to bill the Claude Code subscription login and refuses to guess which credential wins — unset it")
		return 1
	}

	cfg := loadConfig()
	bin, err := exec.LookPath(cfg.ClaudePath)
	if err != nil {
		writeOneShotError(fmt.Sprintf("claude binary not found at %q: %v", cfg.ClaudePath, err))
		return 1
	}

	// --max-turns 1 forbids agentic tool round-trips: a model that tries to
	// use a tool instead of answering ends the run with an error we surface,
	// rather than silently doing work nobody asked a classifier to do.
	args := []string{"-p", "--output-format", "json", "--max-turns", "1"}
	if len(req.Schema) > 0 {
		if !json.Valid(req.Schema) {
			writeOneShotError("request schema is not valid JSON")
			return 1
		}
		// --json-schema is a forced tool call underneath (the CLI reports
		// stop_reason "tool_use"), so this satisfies msg.OneShotRequest's
		// hard-force requirement natively. Measured by the 2026-08-17
		// transport-comparison session (scheduler branch
		// feat/codex-subscription-client, internal/claudecodeheadless).
		args = append(args, "--json-schema", string(req.Schema))
	}
	model := req.Model
	if model == "" {
		model = cfg.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.SystemPrompt != "" {
		// Full replacement, not --append-system-prompt: a classifier wants
		// its own instructions, not Claude Code's agentic preamble.
		args = append(args, "--system-prompt", req.SystemPrompt)
	}

	// Transcripts group under the cwd's project dir. A dedicated directory
	// keeps ~480 machine calls/day from polluting the discover scan of any
	// real repo's session history.
	workDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, ".llm-bridge-claudecode", "oneshot")
		if err := os.MkdirAll(d, 0o755); err == nil {
			workDir = d
		}
	}

	env := os.Environ()
	if req.MaxTokens > 0 {
		env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(req.MaxTokens))
	}

	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = env
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			detail = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		if ctx.Err() == context.DeadlineExceeded {
			writeOneShotError(fmt.Sprintf("claude -p exceeded %s", oneShotTimeout))
			return 1
		}
		writeOneShotError(fmt.Sprintf("claude -p: %v%s (stdout: %s)", err, detail, truncateForError(string(out))))
		return 1
	}

	var envelope struct {
		IsError    bool   `json:"is_error"`
		Result     string `json:"result"`
		StopReason string `json:"stop_reason"`
		NumTurns   int    `json:"num_turns"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		writeOneShotError(fmt.Sprintf("decode claude envelope: %v (stdout: %s)", err, truncateForError(string(out))))
		return 1
	}
	if envelope.IsError {
		writeOneShotError("claude reported an error: " + truncateForError(envelope.Result))
		return 1
	}

	resp := msg.OneShotResponse{
		StopReason: envelope.StopReason,
		DurationMs: time.Since(start).Milliseconds(),
		Model:      model,
		Usage: msg.TokenUsage{
			InputTokens:      envelope.Usage.InputTokens,
			OutputTokens:     envelope.Usage.OutputTokens,
			TotalTokens:      envelope.Usage.InputTokens + envelope.Usage.OutputTokens,
			CacheReadTokens:  envelope.Usage.CacheReadInputTokens,
			CacheWriteTokens: envelope.Usage.CacheCreationInputTokens,
		},
	}

	if len(req.Schema) > 0 {
		parsed, err := extractJSONObject(envelope.Result)
		if err != nil {
			// Includes the truncated-reply case: a reply cut off at the
			// output-token cap is invalid JSON and lands here, loudly.
			writeOneShotError(fmt.Sprintf("reply is not the requested JSON (stop_reason=%q): %v (reply: %s)",
				envelope.StopReason, err, truncateForError(envelope.Result)))
			return 1
		}
		resp.Parsed = parsed
	} else {
		resp.Text = envelope.Result
	}

	json.NewEncoder(os.Stdout).Encode(resp)
	return 0
}

// extractJSONObject returns the reply as raw JSON, tolerating exactly one
// decoration models add despite instructions: a markdown code fence around
// the object. Anything else — prose before the JSON, multiple objects,
// truncation — is an error.
func extractJSONObject(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	var probe json.RawMessage
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&probe); err != nil {
		return nil, err
	}
	// Reject trailing content after the object so "{}\nSome prose" cannot
	// pass as a clean answer.
	if dec.More() {
		return nil, fmt.Errorf("trailing content after JSON object")
	}
	return probe, nil
}

func writeOneShotError(errMsg string) {
	json.NewEncoder(os.Stdout).Encode(map[string]string{"error": errMsg})
}

// truncateForError caps a blob quoted inside an error message. Errors travel
// through HTTP responses and job logs; a full transcript in one would bury
// the message that matters.
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}
