package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// Frames copied from ~/.llm-bridge/bridge.db on 2026-09-17, uuids shortened.

func systemEventOf(t *testing.T, frame string) *msg.SystemEvent {
	t.Helper()
	events := translateEvent(json.RawMessage(frame), "s1", &UsageAggregator{}, nil)
	if len(events) != 1 || events[0].System == nil {
		t.Fatalf("want one system event, got %+v", events)
	}
	return events[0].System
}

func TestTranslateStatus_AutomaticCompactionIsPromotedOffRaw(t *testing.T) {
	running := systemEventOf(t, `{"type":"system","subtype":"status","status":"compacting","session_id":"8312bbe3","uuid":"06f26b97"}`)
	if running.Subtype != "status" || running.Status != msg.SystemStatusCompacting {
		t.Fatalf("compacting frame: got %+v", running)
	}
	ended := systemEventOf(t, `{"type":"system","subtype":"status","status":null,"compact_result":"success","session_id":"8312bbe3","uuid":"de2b31b0"}`)
	if ended.Status != "" || ended.CompactResult != "success" {
		t.Fatalf("closing frame: got %+v", ended)
	}
}

func TestTranslateStatus_PermissionModeFrameReportsNothing(t *testing.T) {
	got := systemEventOf(t, `{"type":"system","subtype":"status","status":null,"permissionMode":"default","uuid":"b708a60c","session_id":"36fd47d4"}`)
	if got.Status != "" || got.CompactResult != "" {
		t.Fatalf("a permission-mode status frame must not read as a compaction: %+v", got)
	}
}

func TestTranslateRateLimit_VerdictIsPromotedOffRaw(t *testing.T) {
	got := systemEventOf(t, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789686000,"rateLimitType":"seven_day","isUsingOverage":false},"uuid":"5e4f9e8c","session_id":"dc80310d"}`)
	if got.Subtype != "rate_limit" || got.RateLimitStatus != msg.RateLimitRejected ||
		got.RateLimitType != "seven_day" || got.RateLimitResetsAt != 1789686000 {
		t.Fatalf("got %+v", got)
	}
}
