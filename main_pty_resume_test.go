package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The pty resume path used to fail silently: when no working directory was
// recorded for the conversation, execClaudePTY changed nothing, said nothing,
// and exec'd claude in whatever directory it had inherited. The user asked to
// resume a conversation and got a working, blank session with no diagnostic
// anywhere. These cases hold the diagnostics in place.
//
// They also hold the one case that must stay silent. A launch with no resume id
// is the ordinary path, and a diagnostic there would be noise on every pty
// session on the box — so "says something" is not the property under test;
// "says something exactly when it cannot do what was asked" is.

// resumeUUID has the 8-4-4-4-12 shape isClaudeSessionUUID requires.
const resumeUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func TestResumeWithNoRecordedDirectoryReportsTheBlankSessionItIsAboutToOpen(t *testing.T) {
	var diagnostics bytes.Buffer
	changed := false

	changeToResumeDirectory(&diagnostics, resumeUUID,
		func(string) (string, bool) { return "", false },
		func(string) error { changed = true; return nil })

	if changed {
		t.Fatal("changed directory despite the lookup reporting not-found")
	}
	said := diagnostics.String()
	if said == "" {
		t.Fatal("not-found produced no diagnostic: this is the silent blank session the card was filed for")
	}
	if !strings.Contains(said, resumeUUID) {
		t.Errorf("diagnostic does not name the resume id, so it cannot be tied to a session: %q", said)
	}
	if !strings.Contains(said, "blank session") {
		t.Errorf("diagnostic does not state the consequence: %q", said)
	}
}

func TestResumeWithAMalformedIDReportsItRatherThanSkippingSilently(t *testing.T) {
	var diagnostics bytes.Buffer
	looked := false

	// The old code gated the whole block on isClaudeSessionUUID, so a resume id
	// that bridge-server set but got wrong took the same silent path as no
	// resume id at all.
	changeToResumeDirectory(&diagnostics, "agent-9f2c11",
		func(string) (string, bool) { looked = true; return "/somewhere", true },
		func(string) error { t.Fatal("changed directory for a malformed resume id"); return nil })

	if looked {
		t.Error("looked up a working directory for an id claude --resume cannot accept")
	}
	said := diagnostics.String()
	if !strings.Contains(said, "agent-9f2c11") {
		t.Errorf("diagnostic does not quote the rejected id, so nobody can see what was sent: %q", said)
	}
	if !strings.Contains(said, "blank session") {
		t.Errorf("diagnostic does not state the consequence: %q", said)
	}
}

func TestResumeChangesIntoTheRecordedDirectoryAndSaysNothing(t *testing.T) {
	var diagnostics bytes.Buffer
	var movedTo string

	changeToResumeDirectory(&diagnostics, resumeUUID,
		func(uuid string) (string, bool) {
			if uuid != resumeUUID {
				t.Errorf("looked up %q, want %q", uuid, resumeUUID)
			}
			return "/home/somebody/repos/widget", true
		},
		func(dir string) error { movedTo = dir; return nil })

	if movedTo != "/home/somebody/repos/widget" {
		t.Errorf("moved to %q, want the recorded directory", movedTo)
	}
	if diagnostics.String() != "" {
		t.Errorf("the success path is not silent: %q", diagnostics.String())
	}
}

func TestAFailedChdirIsReportedWithTheDirectoryAndTheError(t *testing.T) {
	var diagnostics bytes.Buffer

	changeToResumeDirectory(&diagnostics, resumeUUID,
		func(string) (string, bool) { return "/gone", true },
		func(string) error { return errors.New("no such file or directory") })

	said := diagnostics.String()
	for _, want := range []string{"/gone", resumeUUID, "no such file or directory"} {
		if !strings.Contains(said, want) {
			t.Errorf("diagnostic is missing %q: %q", want, said)
		}
	}
}

// The cry-wolf control. A pty launch that is not a resume must reach claude
// with no lookup, no move and nothing on stderr — otherwise the diagnostics
// above are indistinguishable from a function that complains unconditionally.
func TestALaunchThatIsNotAResumeIsSilentAndTouchesNothing(t *testing.T) {
	var diagnostics bytes.Buffer

	changeToResumeDirectory(&diagnostics, "",
		func(string) (string, bool) { t.Fatal("looked up a working directory with no resume id"); return "", false },
		func(string) error { t.Fatal("changed directory with no resume id"); return nil })

	if diagnostics.String() != "" {
		t.Errorf("an ordinary pty launch wrote to stderr: %q", diagnostics.String())
	}
}
