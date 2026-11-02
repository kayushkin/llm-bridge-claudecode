package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIsClaudeSessionUUID guards the resume/fork gate: only a canonical
// 8-4-4-4-12 hex UUID may reach `claude --resume`. Sub-agent rollout ids
// ("agent-<hex>") and bridge session ids must be rejected so the harness
// drops the flag instead of shelling out to a guaranteed-fail `--resume`.
func TestIsClaudeSessionUUID(t *testing.T) {
	valid := []string{
		"a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"00000000-0000-0000-0000-000000000000",
		"A1B2C3D4-E5F6-7890-ABCD-EF1234567890", // uppercase hex
	}
	for _, s := range valid {
		if !isClaudeSessionUUID(s) {
			t.Errorf("isClaudeSessionUUID(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",                                      // empty
		"agent-a245ee69c304b3ee9",               // sub-agent rollout id (the reported failure)
		"agent-a7b7f1f17a9fc339",                // another sub-agent id from the live DB
		"br_17840610686978557",                  // bridge session id
		"agent:claxon:main",                     // inber-style key
		"conformance-abc",                       // conformance harness id
		"a1b2c3d4-e5f6-7890-abcd-ef123456789",   // 35 chars (too short)
		"a1b2c3d4-e5f6-7890-abcd-ef12345678900", // 37 chars (too long)
		"a1b2c3d4e5f67890abcdef1234567890abcd",  // 36 chars but no dashes
		"a1b2c3d4-e5f6-7890-abcd-ef123456789g",  // non-hex digit 'g'
		"a1b2c3d4_e5f6_7890_abcd_ef1234567890",  // underscores, not dashes
		"za1b2c3d-e5f6-7890-abcd-ef1234567890",  // dash in the wrong slot
	}
	for _, s := range invalid {
		if isClaudeSessionUUID(s) {
			t.Errorf("isClaudeSessionUUID(%q) = true, want false", s)
		}
	}
}

// A session stopped before its first turn has a Claude Code UUID but no saved
// conversation, and `claude --resume` on it fails with "No conversation found
// with session ID", losing the message. The harness must start that
// conversation under the same UUID instead, and still resume one that exists.
func TestResumeWithNoSavedConversationStartsItUnderTheSameUUID(t *testing.T) {
	_, restore := swapEmit()
	defer restore()

	for _, saved := range []bool{false, true} {
		path := fakeClaude(t)
		argvLog := filepath.Join(t.TempDir(), "argv")
		t.Setenv("FAKECC_MODE", "prompt")
		t.Setenv("FAKECC_UUID", canaryUUID)
		t.Setenv("FAKECC_ARGV_LOG", argvLog)
		if saved {
			writeTranscript(t, os.Getenv("CLAUDE_CONFIG_DIR"), t.TempDir(), canaryUUID)
		}

		h := NewHarness(&Config{ClaudePath: path, TurnIdleTimeout: 5 * time.Second})
		if err := h.handleStart(StartParams{
			BridgeSessionID:  "canary-bridge",
			SessionID:        "canary-bridge",
			HarnessSessionID: canaryUUID,
			Resume:           true,
			Prompt:           "hello",
		}); err != nil {
			t.Fatalf("saved=%v: handleStart: %v", saved, err)
		}
		if h.proc != nil {
			_ = h.proc.Kill()
		}
		h.cancel()

		argv, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatalf("read argv log: %v", err)
		}
		spawn := strings.TrimSpace(string(argv))
		want, notWant := "--session-id "+canaryUUID, "--resume"
		if saved {
			want, notWant = "--resume "+canaryUUID, "--session-id"
		}
		if !strings.Contains(spawn, want) || strings.Contains(spawn, notWant) {
			t.Fatalf("saved=%v: spawn = %q; want %q and no %q", saved, spawn, want, notWant)
		}
	}
}
