package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The frames below copy the shapes Claude Code 2.1.280 put on stdout in
// session br_1790362479778008348 and in the stream-json reproduction on
// 2026-09-25, trimmed to the fields the stopper reads.

const waiterOutputFile = "/tmp/claude-1000/project/session/tasks/bgtask1.output"

func bashToolUseFrame(toolUseID, command string) json.RawMessage {
	frame, _ := json.Marshal(map[string]any{
		"type":               "assistant",
		"parent_tool_use_id": "toolu_subagent",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_use", "id": toolUseID, "name": "Bash",
			"input": map[string]any{"command": command, "timeout": 250000},
		}}},
	})
	return frame
}

func toolResultFrame(toolUseID string) json.RawMessage {
	frame, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": toolUseID, "content": "done",
		}}},
	})
	return frame
}

func taskStartedFrame(taskID, toolUseID string, isBackgrounded bool) json.RawMessage {
	frame, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_started", "task_id": taskID, "tool_use_id": toolUseID,
		"task_type": "local_bash", "owned_by_subagent": true, "is_backgrounded": isBackgrounded,
	})
	return frame
}

func taskNotificationFrame(taskID, status, outputFile string) json.RawMessage {
	frame, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_notification", "task_id": taskID,
		"status": status, "output_file": outputFile,
	})
	return frame
}

func observeAll(stopper *finishedTaskWaiterStopper, frames ...json.RawMessage) []string {
	var stopped []string
	for _, frame := range frames {
		stopped = append(stopped, stopper.observe(frame)...)
	}
	return stopped
}

func TestFinishedTaskWaiters_StopsForegroundWatcherOfFinishedOutputFile(t *testing.T) {
	stopper := newFinishedTaskWaiterStopper()
	stopped := observeAll(stopper,
		bashToolUseFrame("toolu_bg", "sleep 15; echo FINISHED-MARKER"),
		taskStartedFrame("bgtask1", "toolu_bg", true),
		toolResultFrame("toolu_bg"),
		bashToolUseFrame("toolu_tail", "timeout 200 tail -n +1 -f "+waiterOutputFile),
		taskStartedFrame("waiter1", "toolu_tail", false),
		taskNotificationFrame("bgtask1", "completed", waiterOutputFile),
	)
	if !reflect.DeepEqual(stopped, []string{"waiter1"}) {
		t.Fatalf("stopped %v; want [waiter1]", stopped)
	}
	// A second notice for the same file must not stop it again.
	if again := stopper.observe(taskNotificationFrame("bgtask1", "completed", waiterOutputFile)); len(again) != 0 {
		t.Fatalf("stopped %v a second time", again)
	}
}

func TestFinishedTaskWaiters_StopsWatcherWhenTaskFailed(t *testing.T) {
	stopped := observeAll(newFinishedTaskWaiterStopper(),
		bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
		taskStartedFrame("waiter1", "toolu_tail", false),
		taskNotificationFrame("bgtask1", "failed", waiterOutputFile),
	)
	if !reflect.DeepEqual(stopped, []string{"waiter1"}) {
		t.Fatalf("stopped %v; want [waiter1]", stopped)
	}
}

func TestFinishedTaskWaiters_LeavesOtherCommandsAlone(t *testing.T) {
	cases := map[string][]json.RawMessage{
		"a foreground command that does not name the file": {
			bashToolUseFrame("toolu_build", "go test ./..."),
			taskStartedFrame("build1", "toolu_build", false),
			taskNotificationFrame("bgtask1", "completed", waiterOutputFile),
		},
		"a watcher that is itself in the background": {
			bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
			taskStartedFrame("waiter1", "toolu_tail", true),
			taskNotificationFrame("bgtask1", "completed", waiterOutputFile),
		},
		"a watcher moved to the background after it started": {
			bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
			taskStartedFrame("waiter1", "toolu_tail", false),
			json.RawMessage(`{"type":"system","subtype":"task_updated","task_id":"waiter1","patch":{"is_backgrounded":true}}`),
			taskNotificationFrame("bgtask1", "completed", waiterOutputFile),
		},
		"a watcher that already returned": {
			bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
			taskStartedFrame("waiter1", "toolu_tail", false),
			json.RawMessage(`{"type":"system","subtype":"task_updated","task_id":"waiter1","patch":{"status":"completed"}}`),
			taskNotificationFrame("bgtask1", "completed", waiterOutputFile),
		},
		"a task that was stopped rather than finished": {
			bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
			taskStartedFrame("waiter1", "toolu_tail", false),
			taskNotificationFrame("bgtask1", "stopped", waiterOutputFile),
		},
		"a notice with no output file": {
			bashToolUseFrame("toolu_tail", "tail -f "+waiterOutputFile),
			taskStartedFrame("waiter1", "toolu_tail", false),
			taskNotificationFrame("bgtask1", "completed", ""),
		},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			if stopped := observeAll(newFinishedTaskWaiterStopper(), frames...); len(stopped) != 0 {
				t.Fatalf("stopped %v; want nothing", stopped)
			}
		})
	}
}

func TestFinishedTaskWaiters_ForgetsAnsweredToolUses(t *testing.T) {
	stopper := newFinishedTaskWaiterStopper()
	observeAll(stopper,
		bashToolUseFrame("toolu_quick", "ls"),
		toolResultFrame("toolu_quick"),
	)
	if len(stopper.bashCommandByToolUseID) != 0 {
		t.Fatalf("kept %v after its tool_result", stopper.bashCommandByToolUseID)
	}
}

func TestStopTaskRefusal(t *testing.T) {
	refusedFrame := json.RawMessage(`{"type":"control_response","response":{"subtype":"error","request_id":"stop-finished-task-waiter-1-waiter1","error":"StopTask: not running"}}`)
	requestID, refusal, refused := stopTaskRefusal(refusedFrame)
	if !refused || requestID != "stop-finished-task-waiter-1-waiter1" || refusal != "StopTask: not running" {
		t.Fatalf("got %q %q %v", requestID, refusal, refused)
	}
	for _, frame := range []string{
		`{"type":"control_response","response":{"subtype":"success","request_id":"stop-finished-task-waiter-1-waiter1","response":{}}}`,
		`{"type":"control_response","response":{"subtype":"error","request_id":"int-1","error":"x"}}`,
		`{"type":"system","subtype":"task_notification"}`,
	} {
		if _, _, refused := stopTaskRefusal(json.RawMessage(frame)); refused {
			t.Fatalf("read %s as a refused stop", frame)
		}
	}
}
