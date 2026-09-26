package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claude Code never names its effort in the init frame, and a session that
// was given no --effort runs at a level Claude Code picks per model. The one
// place it states the level it resolved is get_settings: response.applied.effort
// (measured against Claude Code 2.1.282 — "medium" on a bare Opus 5.5 spawn,
// then "high" and "max" after apply_flag_settings). So the harness asks after
// every init and after every effort change, and re-emits SessionInfo with the
// answer.

// reportedEffortRequestIDPrefix marks the get_settings requests this file
// sends, so their answers can be told apart from any other control_response.
const reportedEffortRequestIDPrefix = "session-info-effort-"

// requestReportedEffort asks the live Claude Code process for its settings.
// The answer arrives on the event stream and is read by reportedEffort.
func requestReportedEffort(proc *CCProcess) error {
	requestID := fmt.Sprintf("%s%d", reportedEffortRequestIDPrefix, time.Now().UnixNano())
	return proc.WriteControl(requestID, "get_settings", nil)
}

// reportedEffort returns the effort a get_settings answer to one of our
// requests reports. matched is false for every other frame. A refused request
// returns matched with an error, so the caller logs it. An empty effort with no
// error is a real answer: a model with no effort setting (Haiku 4.5) reports
// "effort": null.
func reportedEffort(raw json.RawMessage) (effort string, matched bool, err error) {
	var frame struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
			Response  struct {
				Applied struct {
					Effort string `json:"effort"`
				} `json:"applied"`
			} `json:"response"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "control_response" {
		return "", false, nil
	}
	if !strings.HasPrefix(frame.Response.RequestID, reportedEffortRequestIDPrefix) {
		return "", false, nil
	}
	if frame.Response.Subtype == "error" {
		return "", true, fmt.Errorf("get_settings %s refused: %s", frame.Response.RequestID, frame.Response.Error)
	}
	return frame.Response.Response.Applied.Effort, true, nil
}
