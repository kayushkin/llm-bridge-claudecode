package main

import (
	"encoding/json"
	"strings"
)

// Claude Code hands a background task's <task-notification> to the agent that
// owns the task only between that agent's tool calls. An agent that starts a
// command in the background and then blocks on a foreground command that
// watches the task's output file — `tail -f <output_file>` is the usual shape,
// since Claude Code refuses a bare foreground `sleep` — never hears that the
// task finished. The notice sits in the queue until the watcher hits its own
// timeout, which can be ten minutes.
//
// Measured on session br_1790362479778008348 on 2026-09-25: a browser-tester
// subagent backgrounded bridge-agent, then ran
// `timeout 580 tail -n +1 -f <its output file>`. bridge-agent finished at
// 19:33:28 and Claude Code emitted task_notification "completed" on stdout at
// that second, but the notice stayed queued until the user interrupted at
// 19:37:57. Reproduced with plain `claude` 2.1.280 and no bridge at all
// (19:57:12 → 20:00:54), so the hold is Claude Code's own behaviour.
//
// Claude Code does register the watcher as a task of its own, and it accepts a
// `stop_task` control request. Stopping the watcher returns its output so far
// (which by then holds the whole report) as the tool result, and the queued
// notice is delivered straight after. Measured on 2.1.280: the stop took 0.1s.
//
// So this stops a foreground Bash task only when its command names the output
// file of a task that has just finished. A watcher of a finished file has
// nothing left to wait for; any other foreground command is left alone.
//
// TestLiveClaudeCodeStillHoldsNoticeBehindFileWatcher checks against the real
// claude binary that the hold still happens. When it fails, Claude Code has
// changed, and this may be unnecessary.
type finishedTaskWaiterStopper struct {
	// bashCommandByToolUseID holds the command of every Bash tool_use not yet
	// answered by a tool_result, from any agent in the process.
	bashCommandByToolUseID map[string]string
	// runningForegroundBashTasks maps the task id of each running,
	// not-backgrounded local_bash task to its tool_use id.
	runningForegroundBashTasks map[string]string
}

func newFinishedTaskWaiterStopper() *finishedTaskWaiterStopper {
	return &finishedTaskWaiterStopper{
		bashCommandByToolUseID:     map[string]string{},
		runningForegroundBashTasks: map[string]string{},
	}
}

type streamFrameForTaskWaiters struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id"`
	TaskType       string `json:"task_type"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	Status         string `json:"status"`
	OutputFile     string `json:"output_file"`
	Patch          struct {
		Status         string `json:"status"`
		IsBackgrounded *bool  `json:"is_backgrounded"`
	} `json:"patch"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlockForTaskWaiters struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     struct {
		Command string `json:"command"`
	} `json:"input"`
}

// observe reads one stream-json frame and returns the ids of the tasks to stop:
// the running foreground Bash tasks whose command names the output file of a
// task this frame reports as finished.
func (s *finishedTaskWaiterStopper) observe(raw json.RawMessage) []string {
	var frame streamFrameForTaskWaiters
	if json.Unmarshal(raw, &frame) != nil {
		return nil
	}
	switch frame.Type {
	case "assistant", "user":
		var blocks []contentBlockForTaskWaiters
		if json.Unmarshal(frame.Message.Content, &blocks) != nil {
			return nil
		}
		for _, block := range blocks {
			switch {
			case block.Type == "tool_use" && block.Name == "Bash":
				s.bashCommandByToolUseID[block.ID] = block.Input.Command
			case block.Type == "tool_result":
				delete(s.bashCommandByToolUseID, block.ToolUseID)
			}
		}
		return nil
	case "system":
	default:
		return nil
	}

	switch frame.Subtype {
	case "task_started":
		if frame.TaskType == "local_bash" && !frame.IsBackgrounded {
			s.runningForegroundBashTasks[frame.TaskID] = frame.ToolUseID
		}
	case "task_updated":
		if frame.Patch.IsBackgrounded != nil && *frame.Patch.IsBackgrounded {
			delete(s.runningForegroundBashTasks, frame.TaskID)
		}
		if frame.Patch.Status != "" && frame.Patch.Status != "running" {
			delete(s.runningForegroundBashTasks, frame.TaskID)
		}
	case "task_notification":
		delete(s.runningForegroundBashTasks, frame.TaskID)
		if frame.Status != "completed" && frame.Status != "failed" {
			return nil
		}
		if frame.OutputFile == "" {
			return nil
		}
		var waiterTaskIDs []string
		for waiterTaskID, waiterToolUseID := range s.runningForegroundBashTasks {
			command, known := s.bashCommandByToolUseID[waiterToolUseID]
			if known && strings.Contains(command, frame.OutputFile) {
				waiterTaskIDs = append(waiterTaskIDs, waiterTaskID)
				delete(s.runningForegroundBashTasks, waiterTaskID)
			}
		}
		return waiterTaskIDs
	}
	return nil
}

// stopTaskRequestIDPrefix marks the stop_task requests this file sends, so their
// answers can be told apart from any other control_response.
const stopTaskRequestIDPrefix = "stop-finished-task-waiter-"

// stopTaskRefusal returns the error of a control_response that refused one of
// our stop_task requests, and false for every other frame.
func stopTaskRefusal(raw json.RawMessage) (requestID, refusal string, refused bool) {
	var frame struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "control_response" {
		return "", "", false
	}
	if !strings.HasPrefix(frame.Response.RequestID, stopTaskRequestIDPrefix) || frame.Response.Subtype != "error" {
		return "", "", false
	}
	return frame.Response.RequestID, frame.Response.Error, true
}
