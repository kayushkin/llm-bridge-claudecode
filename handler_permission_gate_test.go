package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The permission gate a session is started with is carried by two spawn-time
// flags: --settings, which installs bridge-server's PreToolUse hook, and
// --permission-mode. Neither survives a process restart on its own, because
// both are arguments to the `claude` command line and a respawn builds a fresh
// one. Before the fix these were read off the start params, which a respawn
// does not carry, so a session came back with no hook and on the
// bypassPermissions default — unrestricted.
//
// Respawn is not an edge case. This repo's own idle watchdog kills a wedged
// child and says "the next message respawns via --resume", so every wedged turn
// on a gated session silently ungated it.
//
// These tests spawn through the real handleStart and read the real argv the
// harness asked for, using the fake `claude` and the argv log from
// handler_watchdog_canary_test.go.

// ⚠️ Read this before adding a mode to a case table here.
//
// translateCanonicalPermissionMode maps block_all, read, ask_all and custom ALL
// to the CC flag "bypassPermissions" — deliberately, because for those modes the
// bridge prehook is meant to be the sole gate. That is the same string this file
// hands out as the ungated default, so for those four modes --permission-mode is
// IDENTICAL whether the gate survived or was lost. What separates them is
// --settings: with the hook installed, bypassPermissions means "the prehook
// decides"; without it, it means "nothing decides".
//
// So a test that only checks --permission-mode cannot see the regression on the
// modes that matter most. `plan` is the one canonical mode with a distinct flag
// value, which is why it carries the mode assertions below, and `block_all`
// carries the settings assertion with its mode flag explicitly called out as
// proving nothing.

// gateSettings is the shape bridge-server passes: inline JSON naming the
// PreToolUse hook endpoint. Kept free of spaces because the fake logs argv
// space-joined.
const gateSettings = `{"hooks":{"PreToolUse":[{"hooks":[{"type":"http","url":"http://127.0.0.1:8160/permission/cc-prehook/canary-bridge"}]}]}}`

// flagValue returns the value following flag in a space-joined argv line.
func flagValue(t *testing.T, line, flag string) (string, bool) {
	t.Helper()
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == flag {
			if i+1 >= len(fields) {
				t.Fatalf("%s is the last field, it has no value: %q", flag, line)
			}
			return fields[i+1], true
		}
	}
	return "", false
}

// waitForArgvLines polls the fake's argv log until it holds want lines, and
// fails with what it did see if it never does. It also fails if MORE than want
// appear, which would mean a spawn nobody asked for.
func waitForArgvLines(t *testing.T, path string, want int, limit time.Duration) []string {
	t.Helper()
	var lines []string
	deadline := time.Now().Add(limit)
	for {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read argv log: %v", err)
		}
		lines = nil
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		if len(lines) >= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(lines) != want {
		t.Fatalf("expected exactly %d spawns within %s, got %d: %q",
			want, limit, len(lines), lines)
	}
	return lines
}

// startWedgeThenRespawn runs the sequence the watchdog comment describes: a
// cold start that wedges and is killed, then a message that must respawn. It
// returns the two argv lines, cold start first.
//
// respawn selects which of the two respawn sites drives the second spawn:
// "message" for handleMessage, "resume" for handleResume.
func startWedgeThenRespawn(t *testing.T, respawn string, params StartParams) (cold, warm string) {
	t.Helper()
	_, restore := swapEmit()
	t.Cleanup(restore)

	path := fakeClaude(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKECC_MODE", "wedge")
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", argvLog)

	cfg := &Config{ClaudePath: path, TurnIdleTimeout: 2 * time.Second}
	h := NewHarness(cfg)
	t.Cleanup(func() {
		if h.proc != nil {
			_ = h.proc.Kill()
		}
		h.cancel()
	})

	if err := h.handleStart(params); err != nil {
		t.Fatalf("handleStart: %v", err)
	}

	// The watchdog killed the wedged child inline. Alive lags the kill because
	// the OTel receiver holds the spawn goroutine open, and the respawn sites
	// only fire once Alive goes false.
	deadline := time.Now().Add(10 * time.Second)
	for h.proc.Alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.proc.Alive() {
		t.Fatal("the wedged process never went dead; the respawn path was never reached")
	}
	if h.sessionID != canaryUUID {
		t.Fatalf("harness did not adopt the UUID from init: %q", h.sessionID)
	}

	// Point the fake at a healthy turn so the only thing under test is what
	// flags the respawn carries.
	t.Setenv("FAKECC_MODE", "prompt")
	switch respawn {
	case "message":
		if err := h.handleMessage(MessageParams{Content: "still there?"}); err != nil {
			t.Fatalf("handleMessage after a wedged turn: %v", err)
		}
	case "resume":
		if err := h.handleResume(); err != nil {
			t.Fatalf("handleResume after a wedged turn: %v", err)
		}
	default:
		t.Fatalf("unknown respawn site %q", respawn)
	}

	// The fake writes its argv as its first act, but that is the CHILD's first
	// act, and a respawn carrying no prompt returns from handleStart without
	// waiting for a turn. So poll rather than read once: reading immediately
	// races the exec and reports one spawn where there are two.
	lines := waitForArgvLines(t, argvLog, 2, 10*time.Second)
	if !strings.Contains(lines[1], "--resume "+canaryUUID) {
		t.Fatalf("second line is not the respawn: %q", lines[1])
	}
	return lines[0], lines[1]
}

// TestTheRespawnCarriesTheSameSettingsAsTheOriginalSpawn is the assertion that
// matters most: --settings is what installs the gate, and it is the ONLY thing
// separating a gated block_all session from an ungated one (see the note at the
// top of this file).
//
// It asserts the respawn's value EQUALS the cold start's, not merely that some
// value is present. A test for presence alone passes on a default that is not
// this session's gate.
func TestTheRespawnCarriesTheSameSettingsAsTheOriginalSpawn(t *testing.T) {
	cold, warm := startWedgeThenRespawn(t, "message", StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
		Settings:        gateSettings,
		PermissionMode:  msg.PermissionModeBlockAll,
	})

	coldSettings, ok := flagValue(t, cold, "--settings")
	if !ok {
		t.Fatalf("the cold start carried no --settings at all: %q", cold)
	}
	if coldSettings != gateSettings {
		t.Fatalf("cold start --settings = %q, want %q", coldSettings, gateSettings)
	}

	warmSettings, ok := flagValue(t, warm, "--settings")
	if !ok {
		t.Fatalf("the respawn dropped --settings entirely, so the PreToolUse hook "+
			"is not installed and the session is ungated: %q", warm)
	}
	if warmSettings != coldSettings {
		t.Fatalf("respawn --settings = %q, want the original spawn's %q",
			warmSettings, coldSettings)
	}
}

// TestTheRespawnCarriesTheSamePermissionModeAsTheOriginalSpawn uses `plan`,
// the one canonical mode whose CC flag value is not "bypassPermissions", so a
// regression is visible in the flag itself.
func TestTheRespawnCarriesTheSamePermissionModeAsTheOriginalSpawn(t *testing.T) {
	cold, warm := startWedgeThenRespawn(t, "message", StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
		Settings:        gateSettings,
		PermissionMode:  msg.PermissionModePlan,
	})

	coldMode, ok := flagValue(t, cold, "--permission-mode")
	if !ok {
		t.Fatalf("the cold start carried no --permission-mode: %q", cold)
	}
	if coldMode != "plan" {
		t.Fatalf("cold start --permission-mode = %q, want %q", coldMode, "plan")
	}

	warmMode, ok := flagValue(t, warm, "--permission-mode")
	if !ok {
		t.Fatalf("the respawn carried no --permission-mode: %q", warm)
	}
	if warmMode != coldMode {
		t.Fatalf("respawn --permission-mode = %q, want the original spawn's %q",
			warmMode, coldMode)
	}
	if warmMode == "bypassPermissions" {
		t.Fatalf("a session started in plan mode respawned as bypassPermissions: %q", warm)
	}
}

// TestTheResumeSiteCarriesTheGateToo pins the second respawn site. handleResume
// builds its own StartParams the same way handleMessage does, so it dropped the
// same two flags, and a fix applied at one site only would leave this path open.
func TestTheResumeSiteCarriesTheGateToo(t *testing.T) {
	cold, warm := startWedgeThenRespawn(t, "resume", StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
		Settings:        gateSettings,
		PermissionMode:  msg.PermissionModePlan,
	})

	coldSettings, _ := flagValue(t, cold, "--settings")
	warmSettings, ok := flagValue(t, warm, "--settings")
	if !ok || warmSettings != coldSettings {
		t.Fatalf("handleResume respawn --settings = %q (present=%v), want the "+
			"original spawn's %q", warmSettings, ok, coldSettings)
	}

	coldMode, _ := flagValue(t, cold, "--permission-mode")
	warmMode, ok := flagValue(t, warm, "--permission-mode")
	if !ok || warmMode != coldMode {
		t.Fatalf("handleResume respawn --permission-mode = %q (present=%v), want the "+
			"original spawn's %q", warmMode, ok, coldMode)
	}
}

// TestAnUngatedSessionStillDefaultsToBypassPermissions is the control for the
// fix, and it is a real one: the fix moved this default from reading the start
// params to reading the persisted field, and the default itself must not have
// moved with it. A caller naming no mode still gets bypassPermissions on both
// the cold start and the respawn, so CC's own UI never tries to prompt a
// session nobody is watching.
//
// It also proves the two tests above fail for the right reason. If the harness
// had simply stopped emitting --permission-mode, they would pass on absence.
func TestAnUngatedSessionStillDefaultsToBypassPermissions(t *testing.T) {
	cold, warm := startWedgeThenRespawn(t, "message", StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
	})

	for _, tc := range []struct{ name, line string }{
		{"cold start", cold},
		{"respawn", warm},
	} {
		got, present := flagValue(t, tc.line, "--permission-mode")
		if !present {
			t.Fatalf("%s carried no --permission-mode at all: %q", tc.name, tc.line)
		}
		if got != "bypassPermissions" {
			t.Fatalf("%s --permission-mode = %q, want the bypassPermissions default",
				tc.name, got)
		}
		if _, hasSettings := flagValue(t, tc.line, "--settings"); hasSettings {
			t.Fatalf("%s invented a --settings value nobody asked for: %q", tc.name, tc.line)
		}
	}
}
