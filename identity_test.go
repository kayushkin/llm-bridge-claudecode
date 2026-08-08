package main

import (
	"os"
	"os/exec"
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
// installed binary. Four of the five shipped wrong in llm-bridge-copilotcli (a
// claudecode clone) and none was caught by a build or a test.
//
// The count is four, recounted on 2026-08-08 from copilotcli's initial commit
// 0ec0dbd rather than inherited: go.mod and DefaultStatePath (both fixed by
// 154c948), deploy.sh BIN_NAME (c51e880) and the harness constant. It read three
// until then, having missed the BIN_NAME collision. It is a claim about what
// SHIPPED, so it does not fall as the legs are fixed — all four are repaired now
// and the number is still four.
//
// The checkout directory is the anchor because it is the only one of the five a
// clone does not inherit from its parent.
//
// The anchor is the name of the repository's MAIN working tree, not of the
// directory the test happens to run in. Those differ inside a linked git
// worktree, and taking the running directory there made this test report every
// leg wrong against a tree that was entirely correct: a worktree named
// llm-bridge-<harness>-wt-<topic> satisfies the llm-bridge- prefix, so the skip
// below did not fire and each constant was compared against the worktree's name.
// A test that cries wolf wherever you work is one a reader learns to ignore, and
// this test exists to catch a clone that was never re-targeted.
func TestHarnessIdentityIsSelfConsistent(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want, ok := mainWorkingTreeName(root)
	if !ok {
		t.Skipf("%s is not a git working tree, so there is no canonical checkout name to anchor identity against", root)
	}
	if !strings.HasPrefix(want, "llm-bridge-") {
		t.Skipf("repository directory %q is not a canonical llm-bridge-* checkout; no anchor to compare identity against", want)
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

// mainWorkingTreeName returns the directory name of the repository's main working
// tree, given any directory inside it. The second result is false when dir is not
// inside a git working tree at all, which is the only case with no anchor to
// compare identity against.
//
// git reports the shared git directory as ".git" (relative) from a normal checkout
// and as "<main>/.git" (absolute) from a linked worktree, so resolving it against
// dir and taking its parent yields the main working tree in both cases. There is
// deliberately no branch on whether this is a worktree: a second code path is a
// second thing to get wrong, and this bug was a missing case, not a wrong one.
func mainWorkingTreeName(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", false
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return "", false
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}
	return filepath.Base(filepath.Dir(commonDir)), true
}

// TestMainWorkingTreeNameIsWorktreeInvariant pins the property the identity test
// depends on and nothing else asserts: the anchor is the same value read from a
// checkout and from a linked worktree of it, whatever the worktree is called.
//
// Without this, the only thing keeping the identity anchor worktree-invariant is
// the doc comment above it, and the defect it replaced was reintroduced simply by
// reading the running directory -- the obvious thing to write.
func TestMainWorkingTreeNameIsWorktreeInvariant(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH, so the anchor cannot be resolved: %v", err)
	}
	base := t.TempDir()
	checkout := filepath.Join(base, "llm-bridge-fixture")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	git(checkout, "init", "-q")
	if err := os.WriteFile(filepath.Join(checkout, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	git(checkout, "add", "f")
	git(checkout, "commit", "-qm", "seed")

	// The worktree name carries the llm-bridge- prefix on purpose: that is exactly
	// what let the old anchor through the skip guard, so a fix that only widened
	// the guard would still fail here.
	worktree := filepath.Join(base, "llm-bridge-fixture-wt-topic")
	git(checkout, "worktree", "add", "-q", worktree, "-b", "topic")

	fromCheckout, ok := mainWorkingTreeName(checkout)
	if !ok {
		t.Fatalf("mainWorkingTreeName(%q) found no git working tree", checkout)
	}
	if fromCheckout != "llm-bridge-fixture" {
		t.Errorf("anchor read from the checkout = %q, want %q", fromCheckout, "llm-bridge-fixture")
	}

	fromWorktree, ok := mainWorkingTreeName(worktree)
	if !ok {
		t.Fatalf("mainWorkingTreeName(%q) found no git working tree", worktree)
	}
	if fromWorktree != fromCheckout {
		t.Errorf("anchor read from linked worktree %q = %q, want %q (the main working tree's name)\n"+
			"the identity legs are all compared against this, so a worktree-relative anchor fails every one of them",
			filepath.Base(worktree), fromWorktree, fromCheckout)
	}

	if _, ok := mainWorkingTreeName(base); ok {
		t.Errorf("mainWorkingTreeName(%q) reported a working tree outside any repository", base)
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
