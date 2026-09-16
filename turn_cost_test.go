package main

import (
	"encoding/json"
	"math"
	"testing"
)

func resultFrame(totalCostUSD float64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "result", "total_cost_usd": totalCostUSD, "usage": map[string]any{}})
	return b
}

// Claude Code reports total_cost_usd cumulatively for the CLI process; each
// result event must carry only what that turn added.
func TestFinalizeReportsEachTurnsCostNotTheRunningTotal(t *testing.T) {
	var agg UsageAggregator
	want := []struct {
		cumulative, turn float64
		priced           bool
	}{
		{2.25, 2.25, true},
		{2.96, 0.71, true},
		{2.96, 0, false},  // a turn that added nothing reports no cost
		{0.40, 0.40, true}, // the CLI process restarted: its whole total is this turn's
		{1.00, 0.60, true},
		{0, 0, false}, // an unpriced turn leaves the running total alone
		{1.50, 0.50, true},
	}
	for i, step := range want {
		_, cost := agg.Finalize(resultFrame(step.cumulative))
		agg.Reset() // what the handler does between turns
		if !step.priced {
			if cost != nil {
				t.Fatalf("step %d: cost %+v, want none", i, *cost)
			}
			continue
		}
		if cost == nil || math.Abs(cost.TotalUSD-step.turn) > 1e-9 {
			t.Fatalf("step %d: cost %+v, want %.2f", i, cost, step.turn)
		}
	}
}
