package main

import (
	"encoding/json"
	"testing"
)

// TestReportedEffortReadsOnlyOurGetSettingsAnswers covers the three answers
// Claude Code 2.1.282 gave on a live process — a level, null for Haiku 4.5,
// and a refusal — plus a control_response to someone else's request.
func TestReportedEffortReadsOnlyOurGetSettingsAnswers(t *testing.T) {
	cases := []struct {
		name        string
		frame       string
		wantEffort  string
		wantMatched bool
		wantErr     bool
	}{
		{
			name:        "a level",
			frame:       `{"type":"control_response","response":{"subtype":"success","request_id":"session-info-effort-1","response":{"applied":{"model":"claude-opus-5-5","effort":"medium"}}}}`,
			wantEffort:  "medium",
			wantMatched: true,
		},
		{
			name:        "a model with no effort setting",
			frame:       `{"type":"control_response","response":{"subtype":"success","request_id":"session-info-effort-2","response":{"applied":{"model":"claude-haiku-4-5-20251001","effort":null}}}}`,
			wantMatched: true,
		},
		{
			name:        "a refusal",
			frame:       `{"type":"control_response","response":{"subtype":"error","request_id":"session-info-effort-3","error":"nope"}}`,
			wantMatched: true,
			wantErr:     true,
		},
		{
			name:  "another request's answer",
			frame: `{"type":"control_response","response":{"subtype":"success","request_id":"ctl-4","response":{"applied":{"effort":"high"}}}}`,
		},
		{
			name:  "not a control_response",
			frame: `{"type":"assistant","message":{}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			effort, matched, err := reportedEffort(json.RawMessage(c.frame))
			if effort != c.wantEffort || matched != c.wantMatched || (err != nil) != c.wantErr {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, err=%v)", effort, matched, err, c.wantEffort, c.wantMatched, c.wantErr)
			}
		})
	}
}
