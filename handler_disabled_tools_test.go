package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// llm-bridge-server sends the session's list as harness_config.disabled_tools,
// copied verbatim into the start params. Before this field was named for it,
// the wrapper read disallowed_tools, so nothing the server sent disabled
// anything.
func TestStartParamsReadDisabledToolsUnderTheServersName(t *testing.T) {
	var params StartParams
	if err := json.Unmarshal([]byte(`{"disabled_tools":["Bash","WebSearch"]}`), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(params.DisabledTools) != 2 || params.DisabledTools[0] != "Bash" || params.DisabledTools[1] != "WebSearch" {
		t.Fatalf("DisabledTools = %v, want [Bash WebSearch]", params.DisabledTools)
	}
}

func TestDisallowedToolsArgsBindsOneNamePerFlag(t *testing.T) {
	got := disallowedToolsArgs([]string{"Bash", "mcp__x__y"})
	want := []string{"--disallowed-tools=Bash", "--disallowed-tools=mcp__x__y"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if len(disallowedToolsArgs(nil)) != 0 {
		t.Fatalf("no disabled tools must add no flag")
	}
}

// The list must reach both the first spawn and the respawn a later message
// triggers, which builds its own StartParams without the list.
func TestCanaryDisabledToolsReachStartAndRespawn(t *testing.T) {
	_, restore := swapEmit()
	defer restore()

	path := fakeClaude(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKECC_MODE", "prompt")
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", argvLog)

	cfg := &Config{ClaudePath: path, TurnIdleTimeout: 5 * time.Second}
	h := NewHarness(cfg)
	t.Cleanup(func() {
		if h.proc != nil {
			_ = h.proc.Kill()
		}
		h.cancel()
	})

	if err := h.handleStart(StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
		DisabledTools:   []string{"Bash", "Read"},
	}); err != nil {
		t.Fatalf("handleStart: %v", err)
	}

	if err := h.proc.Kill(); err != nil {
		t.Fatalf("kill first process: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.proc.Alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.proc.Alive() {
		t.Fatalf("first process still alive after kill")
	}

	if err := h.handleMessage(MessageParams{Content: "again"}); err != nil {
		t.Fatalf("handleMessage respawn: %v", err)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two spawns, got %d: %q", len(lines), lines)
	}
	for i, line := range lines {
		if !strings.Contains(line, "--disallowed-tools=Bash --disallowed-tools=Read") {
			t.Fatalf("spawn %d lacks the disabled tools: %q", i+1, line)
		}
	}
	if !strings.Contains(lines[1], "--resume "+canaryUUID) {
		t.Fatalf("second spawn was not the respawn: %q", lines[1])
	}
}
