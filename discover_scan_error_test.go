package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// captureLog redirects the standard logger into a buffer for the duration of
// the test and restores the previous output, flags and prefix afterwards.
//
// The buffer is the thing under test here, not scaffolding: "the failure is
// reported" is the behaviour this file exists to pin, and the only place that
// report exists is the log. Asserting on it is therefore a real assertion, not
// a reach guard on a precondition.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevFlags, prevPrefix := log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return &buf
}

// shortUserLine is a well-formed CC user record, comfortably under any ceiling.
func shortUserLine(n int) string {
	return `{"type":"user","timestamp":"2026-04-30T12:00:0` +
		strconv.Itoa(n%10) + `Z","message":{"content":"turn ` + strconv.Itoa(n) + `"}}`
}

// TestParseSessionHeadReportsAnOverLongLineInsteadOfDroppingItInSilence pins the
// half of the ceiling behaviour that TestParseSessionHeadReadsTheLongestLineItsCeilingAllows
// deliberately does not: not WHAT is dropped, but whether anything says so.
//
// bufio.Scanner ends a scan on ErrTooLong exactly as it ends one at EOF —
// Scan() returns false — so before this was fixed a session with five user
// turns behind one long line was indistinguishable from a session with two
// turns and nothing else in it. The count was wrong and confident.
//
// ⚠️ The loss is not limited to the over-long line. The scan stops there, so
// every line after it is unread as well. Measured directly against bufio with
// the shipped buffer shape (make([]byte, 0, 256*1024), 1024*1024):
//
//	lines before   lines after   delivered   lost
//	           0           500           0    501
//	           3           500           3    501
//	         250           250         250    251
//
// That is the difference from logstack's identical swallow, where a cursor
// advanced into the middle of the long line and the entries after it arrived on
// the next poll. parseSessionHead is one-shot: there is no next poll, and
// nothing after the long line is ever read.
func TestParseSessionHeadReportsAnOverLongLineInsteadOfDroppingItInSilence(t *testing.T) {
	const (
		overCeiling  = 1048576 // exactly the shipped cap, which bufio refuses
		turnsBefore  = 2
		turnsAfter   = 3
		wantAllTurns = turnsBefore + turnsAfter
	)

	build := func(t *testing.T, withOverLongLine bool) string {
		t.Helper()
		var b strings.Builder
		for i := 0; i < turnsBefore; i++ {
			b.WriteString(shortUserLine(i) + "\n")
		}
		if withOverLongLine {
			b.WriteString(jsonlLineOfExactly(t, overCeiling) + "\n")
		}
		for i := 0; i < turnsAfter; i++ {
			b.WriteString(shortUserLine(turnsBefore+i) + "\n")
		}
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		return path
	}

	// The control comes first and carries most of the weight. Without it, a
	// mutation that logs on every call — or one that reports a truncation that
	// never happened — passes the interesting case below and is never seen.
	t.Run("a file with no over-long line counts every turn and says nothing", func(t *testing.T) {
		logged := captureLog(t)
		path := build(t, false)

		_, _, turns := parseSessionHead(path)

		if turns != wantAllTurns {
			t.Errorf("turns = %d, want %d: every line in this fixture is under the ceiling", turns, wantAllTurns)
		}
		if logged.Len() != 0 {
			t.Errorf("nothing was truncated, so nothing should have been logged; got:\n%s", logged.String())
		}
	})

	t.Run("an over-long line truncates the count and is reported", func(t *testing.T) {
		logged := captureLog(t)
		path := build(t, true)

		_, _, turns := parseSessionHead(path)

		// The turns after the long line are gone. This is the wrong-and-confident
		// number the report exists to explain.
		if turns != turnsBefore {
			t.Errorf("turns = %d, want %d: the scan stops at the over-long line, so the %d turns after it are unread",
				turns, turnsBefore, turnsAfter)
		}
		if turns == wantAllTurns {
			t.Errorf("turns = %d: the over-long line did not truncate the scan at all, so this fixture "+
				"no longer reaches the behaviour under test", turns)
		}

		out := logged.String()
		if out == "" {
			t.Fatalf("the scan stopped early and nothing was logged: the drop is silent again, "+
				"which is the whole defect this test pins (turns reported: %d of %d)", turns, wantAllTurns)
		}
		// Name the file: a report that cannot be traced to a session is not
		// actionable, and a bare "scan error" line is indistinguishable between
		// the hundreds of files a cold import walks.
		if !strings.Contains(out, path) {
			t.Errorf("the report does not name the session file, so it cannot be traced back to one.\nwant substring: %s\ngot:\n%s", path, out)
		}
		if !strings.Contains(out, "too long") {
			t.Errorf("the report does not say why the scan stopped.\ngot:\n%s", out)
		}
		// The partial count has to appear, otherwise the reader is told a scan
		// broke but not that the number alongside it is short.
		if !strings.Contains(out, strconv.Itoa(turnsBefore)) {
			t.Errorf("the report does not carry the partial turn count (%d), so nothing connects it to the wrong number.\ngot:\n%s", turnsBefore, out)
		}
	})
}
