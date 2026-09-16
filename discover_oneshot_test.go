package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestPathToCCProjectMatchesClaudeCode pins the encoding against the names
// Claude Code actually writes. The dotted case is the one this used to get
// wrong: ~/.llm-bridge-claudecode/oneshot lives on this host under
// -home-kayushkincom--llm-bridge-claudecode-oneshot, with '/.' folded to "--".
func TestPathToCCProjectMatchesClaudeCode(t *testing.T) {
	cases := map[string]string{
		"/home/u/repos":                          "-home-u-repos",
		"/home/u/.llm-bridge-claudecode/oneshot": "-home-u--llm-bridge-claudecode-oneshot",
		"/home/u/my_repo v2":                     "-home-u-my-repo-v2",
		"/tmp/é":                                 "-tmp--",
		"/tmp/😀":                                 "-tmp---",
	}
	for in, want := range cases {
		if got := pathToCCProject(in); got != want {
			t.Errorf("pathToCCProject(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDiscover_OneshotTranscriptsCarryOneshotPurpose confirms that a transcript
// left by oneshot mode is discovered with Source=oneshot and the working
// directory as its project, that an ordinary session in a sibling project is
// left untagged, and that a subagent layout under the oneshot project still
// classifies as a subagent — the structural classification wins.
func TestDiscover_OneshotTranscriptsCarryOneshotPurpose(t *testing.T) {
	projectsDir, home := withCCHome(t)

	oneshotDirectory, err := oneshotWorkingDirectory()
	if err != nil {
		t.Fatalf("oneshotWorkingDirectory: %v", err)
	}
	if want := filepath.Join(home, ".llm-bridge-claudecode", "oneshot"); oneshotDirectory != want {
		t.Fatalf("oneshotWorkingDirectory = %q, want %q under the temp HOME", oneshotDirectory, want)
	}
	oneshotProject := filepath.Join(projectsDir, pathToCCProject(oneshotDirectory))

	writeJSONL(t, filepath.Join(oneshotProject, "uuid-oneshot.jsonl"), "classify this")
	writeJSONL(t, filepath.Join(oneshotProject, "uuid-oneshot", "subagents", "agent-inside.jsonl"), "sub")
	// Differs from the oneshot directory only by the dot, which the encoding
	// turns into one extra '-'. A match that ignored the dot would tag it.
	writeJSONL(t, filepath.Join(projectsDir, pathToCCProject(filepath.Join(home, "llm-bridge-claudecode", "oneshot")), "uuid-lookalike.jsonl"), "not oneshot")
	writeJSONL(t, filepath.Join(projectsDir, "-tmp-proj", "uuid-chat.jsonl"), "regular chat")

	got, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	bySource := map[string]string{}
	byProject := map[string]string{}
	for _, s := range got {
		bySource[s.HarnessSessionID] = s.Source
		byProject[s.HarnessSessionID] = s.Project
	}

	if bySource["uuid-oneshot"] != msg.PurposeOneshot {
		t.Errorf("oneshot transcript Source = %q, want %q (all: %v)", bySource["uuid-oneshot"], msg.PurposeOneshot, bySource)
	}
	if byProject["uuid-oneshot"] != oneshotDirectory {
		t.Errorf("oneshot transcript Project = %q, want the working directory %q", byProject["uuid-oneshot"], oneshotDirectory)
	}
	if bySource["agent-inside"] != msg.PurposeSubagent {
		t.Errorf("subagent under the oneshot project Source = %q, want %q", bySource["agent-inside"], msg.PurposeSubagent)
	}
	if bySource["uuid-lookalike"] != "" {
		t.Errorf("lookalike project Source = %q, want empty", bySource["uuid-lookalike"])
	}
	if bySource["uuid-chat"] != "" {
		t.Errorf("ordinary session Source = %q, want empty", bySource["uuid-chat"])
	}
}

// TestOneshotWorkingDirectoryFailsWithoutHome: with no home directory there is
// no oneshot directory, and discovery must say so rather than classify nothing.
func TestOneshotWorkingDirectoryFailsWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("os.UserHomeDir resolves without HOME on this platform")
	}
	if _, err := oneshotWorkingDirectory(); err == nil {
		t.Fatal("oneshotWorkingDirectory succeeded with no home directory")
	}
}
