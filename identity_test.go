package main

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestHarnessIdentityIsSelfConsistent pins every place this wrapper commits to a
// harness identity to a single value: the harness this checkout actually is.
//
//	checkout directory name        which harness this repo is
//	module path in go.mod          who we are as a Go package
//	msg.HarnessBinaryName(harness) the binary name llm-bridge-server resolves on PATH
//	DefaultStatePath() directory   whose session chain we read and write
//	BIN_NAME in deploy.sh          what ./deploy.sh builds, installs and restarts
//
// These are five independent constants. Every wrapper in this family was created
// by cloning an existing one, which copies all five verbatim, and any one left
// un-retargeted silently points this harness at a different harness: it answers
// to that harness's name on the bus, writes into its state.db, or overwrites its
// installed binary. Three of the five shipped wrong in llm-bridge-copilotcli (a
// claudecode clone) and none was caught by a build or a test.
//
// The checkout directory is the anchor because it is the only one of the five a
// clone does not inherit from its parent.
func TestHarnessIdentityIsSelfConsistent(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := filepath.Base(root)
	if !strings.HasPrefix(want, "llm-bridge-") {
		t.Skipf("checkout dir %q is not a canonical llm-bridge-* checkout; no anchor to compare identity against", want)
	}

	if got := msg.HarnessBinaryName(harness); got != want {
		t.Errorf("harness constant is %q, whose binary name is %q, want %q\n"+
			"this wrapper stamps %q on every event it emits and every session it discovers",
			harness, got, want, harness)
	}

	if got := moduleBase(t, root); got != want {
		t.Errorf("go.mod module path base = %q, want %q", got, want)
	}

	if got := filepath.Base(filepath.Dir(DefaultStatePath())); got != want {
		t.Errorf("DefaultStatePath() = %q, so the state dir is %q, want %q\n"+
			"a wrong state dir means this harness reads and writes another harness's session chain",
			DefaultStatePath(), got, want)
	}

	if got, ok := deployBinName(t, root); ok && got != want {
		t.Errorf("deploy.sh BIN_NAME = %q, want %q\n"+
			"./deploy.sh would install this build over the %q binary and restart its service",
			got, want, got)
	}
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// moduleBase returns the last path element of the module path declared in go.mod.
func moduleBase(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := moduleLine.FindSubmatch(b)
	if m == nil {
		t.Fatalf("no module line in go.mod")
	}
	return path.Base(string(m[1]))
}

var binNameLine = regexp.MustCompile(`(?m)^BIN_NAME=["']?([^"'\s]+)`)

// deployBinName returns the BIN_NAME deploy.sh installs under. The second result
// is false when this wrapper ships no deploy.sh, or none that sets BIN_NAME.
func deployBinName(t *testing.T, root string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "deploy.sh"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read deploy.sh: %v", err)
	}
	m := binNameLine.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}
