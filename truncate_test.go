package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The defect these tests pin: cutting a Go string at a fixed byte offset splits
// whatever rune straddles that offset, and the result is not valid UTF-8.
// encoding/json substitutes U+FFFD rather than failing, so nothing along the
// path reports it.
//
// A single hand-picked input is not enough. Whether a cut splits a rune depends
// on where that rune happens to sit relative to the budget, so one string lands
// off the boundary and passes against the unfixed code — which is how this
// survived in twenty repos. Every test here slides the cut across a range and
// states how much of that range actually exercised the case.

// musicalNote is four bytes (U+1D11E). A two-byte rune is a weaker probe: only
// one of its two interior offsets is wrong, so a test that picks the other one
// is green against the bug.
const musicalNote = "\U0001D11E"

// TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset walks maxBytes across
// the whole length of an all-four-byte string. Three offsets in every four
// straddle a rune; the fourth is boundary-aligned and is the known-negative
// control, which must come back untouched against fixed and unfixed code alike.
func TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset(t *testing.T) {
	s := strings.Repeat(musicalNote, 50) // 200 bytes, the caller's real budget
	straddled, aligned := 0, 0

	for maxBytes := 1; maxBytes <= len(s); maxBytes++ {
		got := truncateAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		if len(got) > maxBytes {
			t.Fatalf("maxBytes=%d: result is %d bytes, over budget", maxBytes, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("maxBytes=%d: result is not a prefix of the input: %q", maxBytes, got)
		}

		if maxBytes%4 == 0 {
			// Boundary-aligned control: nothing straddles the cut, so a byte
			// cut and a rune-safe cut agree. This must NOT depend on the fix.
			aligned++
			if len(got) != maxBytes {
				t.Fatalf("maxBytes=%d is rune-aligned, want the full %d bytes, got %d",
					maxBytes, maxBytes, len(got))
			}
			continue
		}

		straddled++
		// Maximality. Valid-and-within-budget alone is not falsifiable: a helper
		// that returns "" for everything satisfies both. Pin that no more of the
		// input could have fitted, which is what a trims-to-nothing or an
		// overshoot-by-one mutation breaks.
		if lost := maxBytes - len(got); lost >= 4 {
			t.Fatalf("maxBytes=%d: lost %d bytes, more than the %d-byte rune straddling the cut",
				maxBytes, lost, 4)
		}
		if len(got) < len(s) {
			_, width := utf8.DecodeRuneInString(s[len(got):])
			if len(got)+width <= maxBytes {
				t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
					maxBytes, len(got), width)
			}
		}
	}

	// A sliding test is a claim that it covered a range, and nothing checks that
	// claim unless it is written down. An early version of this loop moved the
	// input rather than the cut, never straddled once, and was green against the
	// unfixed helper.
	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	if aligned == 0 {
		t.Fatal("the known-negative control never ran: cannot tell a real catch from a false one")
	}
	t.Logf("cut straddled a rune at %d of %d offsets (%d aligned controls)",
		straddled, straddled+aligned, aligned)
}

// TestTruncateAtRuneBoundaryMixedWidths repeats the slide over a string mixing
// one-, two-, three- and four-byte runes, so the result does not depend on every
// rune being the same width.
func TestTruncateAtRuneBoundaryMixedWidths(t *testing.T) {
	s := strings.Repeat("aé€"+musicalNote, 25) // 1+2+3+4 bytes per group
	straddled := 0

	for maxBytes := 1; maxBytes <= len(s); maxBytes++ {
		got := truncateAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		if len(got) > maxBytes {
			t.Fatalf("maxBytes=%d: result is %d bytes, over budget", maxBytes, len(got))
		}
		if len(got) < len(s) {
			_, width := utf8.DecodeRuneInString(s[len(got):])
			if len(got)+width <= maxBytes {
				t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
					maxBytes, len(got), width)
			}
		}
		if len(got) < maxBytes {
			straddled++
		}
	}

	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	t.Logf("cut straddled a rune at %d of %d offsets", straddled, len(s))
}

func TestTruncateAtRuneBoundaryEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		// ASCII is the other known-negative control: the whole class is a no-op
		// on it, so these must hold against the unfixed helper too.
		{"ascii under budget", "hello", 200, "hello"},
		{"ascii exactly at budget", "hello", 5, "hello"},
		{"ascii over budget cuts plainly", "hello", 3, "hel"},
		{"empty input", "", 200, ""},
		{"zero budget", "hello", 0, ""},
		{"negative budget", "hello", -1, ""},
		// One four-byte rune against a budget too small to hold it: the only
		// rune-safe answer is the empty string, not a lone lead byte.
		{"budget smaller than the first rune", musicalNote, 3, ""},
		{"budget exactly the first rune", musicalNote, 4, musicalNote},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateAtRuneBoundary(c.in, c.maxBytes); got != c.want {
				t.Fatalf("truncateAtRuneBoundary(%q, %d) = %q, want %q",
					c.in, c.maxBytes, got, c.want)
			}
		})
	}
}

// TestDiscoveredPromptStaysValidUTF8 is the call-site test. The helper being
// correct does not prove the caller uses it, and the caller is what makes this
// tier 1 rather than cosmetic: the label is encoded into msg.StoredSession and
// crosses to bridge-server, so a split rune survives the request.
//
// The prompt slides so that a four-byte rune lands on each of the four byte
// offsets around the 200-byte budget, and the assertion is made on the JSON
// bridge-server actually receives.
func TestDiscoveredPromptStaysValidUTF8(t *testing.T) {
	for pad := 197; pad <= 200; pad++ {
		projectsDir, _ := withCCHome(t)
		prompt := strings.Repeat("a", pad) + musicalNote + strings.Repeat("b", 50)
		writeJSONL(t, filepath.Join(projectsDir, "-tmp-proj", "uuid-a.jsonl"), prompt)

		got, err := discoverSessions("")
		if err != nil {
			t.Fatalf("pad=%d: discoverSessions: %v", pad, err)
		}
		if len(got) != 1 {
			t.Fatalf("pad=%d: want 1 session, got %d", pad, len(got))
		}

		if !utf8.ValidString(got[0].Prompt) {
			t.Fatalf("pad=%d: discovered prompt is not valid UTF-8: %q", pad, got[0].Prompt)
		}
		if len(got[0].Prompt) > 200 {
			t.Fatalf("pad=%d: prompt is %d bytes, over the 200-byte budget", pad, len(got[0].Prompt))
		}
		// The byte cut does not fail here, it corrupts silently: json.Marshal
		// swaps the split rune for U+FFFD and reports no error. Assert on the
		// encoded form so the test sees what bridge-server sees.
		encoded, err := json.Marshal(got[0].Prompt)
		if err != nil {
			t.Fatalf("pad=%d: marshal: %v", pad, err)
		}
		if strings.Contains(string(encoded), `�`) {
			t.Fatalf("pad=%d: prompt reached JSON carrying U+FFFD: %s", pad, encoded)
		}
		os.RemoveAll(projectsDir)
	}
}

// TestDiscoveredLabelKeepsExactlyTheShippedBudget pins the 200 at the call site
// — the only budget in this file that ships. Every other test here supplies its
// own maxBytes, so none of them is a claim about the number production uses; the
// suite above bounds the label from ABOVE ("over the 200-byte budget") and never
// from below, which leaves the shipped budget free to shrink to any smaller
// value with the whole suite green. Measured by sabotage: 200 -> 201 was caught
// by the length bound, 200 -> 199 was UNNOTICED.
//
// The assertion is on which characters survive, not on a count. Byte 200 is the
// last one inside the budget and byte 201 the first one outside it, each marked
// with a distinct ASCII character, so the test straddles the boundary without
// repeating the number under test — a length assertion would have to spell 200
// again, and a test that spells the constant it is meant to pin moves with it.
//
// Both markers are ASCII on purpose: this test is about the number and must not
// depend on the rune mechanism the rest of the file exercises.
func TestDiscoveredLabelKeepsExactlyTheShippedBudget(t *testing.T) {
	const lastByteInsideBudget = "~"
	const firstByteOutsideBudget = "^"

	projectsDir, _ := withCCHome(t)
	prompt := strings.Repeat("a", 199) + lastByteInsideBudget + firstByteOutsideBudget +
		strings.Repeat("b", 50)
	writeJSONL(t, filepath.Join(projectsDir, "-tmp-proj", "uuid-a.jsonl"), prompt)

	got, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}

	label := got[0].Prompt
	if !strings.HasSuffix(label, lastByteInsideBudget) {
		t.Fatalf("the label dropped the last byte inside the budget: the shipped budget is smaller than it was: %q",
			label)
	}
	if strings.Contains(label, firstByteOutsideBudget) {
		t.Fatalf("the label kept the first byte outside the budget: the shipped budget is larger than it was: %q",
			label)
	}
}
