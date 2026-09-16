package main

import (
	"encoding/json"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// UsageAggregator accumulates per-API-call token usage across a multi-turn run.
type UsageAggregator struct {
	calls     []msg.TokenUsage
	toolCalls int

	// cumulativeCostUSD is the last `total_cost_usd` the CLI reported. Claude
	// Code's figure is cumulative for the CLI process, and msg.ResultEvent.Cost
	// is one turn's cost, so each result emits the difference. Not cleared by
	// Reset, which runs between turns: it lives as long as the process does.
	cumulativeCostUSD float64
}

// ccAssistantUsage is the usage block inside a CC assistant message.
type ccAssistantUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ccResultUsage is the aggregate usage in a CC result event.
type ccResultUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ccModelUsageEntry is a per-model breakdown in the result's modelUsage map.
type ccModelUsageEntry struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
}

// AddAPICall records token usage from a single CC assistant event.
func (a *UsageAggregator) AddAPICall(usage ccAssistantUsage) {
	a.calls = append(a.calls, msg.TokenUsage{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	})
}

// AddToolCall increments the tool call counter.
func (a *UsageAggregator) AddToolCall() {
	a.toolCalls++
}

// Finalize builds the canonical TokenUsage and Cost from a CC result event.
// The result event's own usage/modelUsage is the source of truth for aggregates.
func (a *UsageAggregator) Finalize(raw json.RawMessage) (msg.TokenUsage, *msg.Cost) {
	var result struct {
		Usage      ccResultUsage                     `json:"usage"`
		ModelUsage map[string]ccModelUsageEntry       `json:"modelUsage"`
		TotalCost  float64                            `json:"total_cost_usd"`
	}
	_ = json.Unmarshal(raw, &result)

	usage := msg.TokenUsage{
		InputTokens:      result.Usage.InputTokens,
		OutputTokens:     result.Usage.OutputTokens,
		CacheReadTokens:  result.Usage.CacheReadInputTokens,
		CacheWriteTokens: result.Usage.CacheCreationInputTokens,
		TotalTokens:      result.Usage.InputTokens + result.Usage.OutputTokens,
	}

	// ContextLimit: model's context window from modelUsage (last entry wins if multi-model).
	// CC reports the base 200k window even for the 1M extended-context beta, so when the
	// model id advertises the [1m] variant we use its true 1M limit instead.
	for model, mu := range result.ModelUsage {
		limit := mu.ContextWindow
		if strings.Contains(model, "[1m]") && limit < 1_000_000 {
			limit = 1_000_000
		}
		usage.ContextLimit = limit
	}

	// ContextTokens: snapshot of the final API call's input (input + cache read + cache write).
	// modelUsage sums across every API call in the run, so it cannot represent a context-window snapshot.
	if n := len(a.calls); n > 0 {
		last := a.calls[n-1]
		usage.ContextTokens = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
	}

	return usage, a.turnCost(result.TotalCost)
}

// turnCost converts the CLI's cumulative `total_cost_usd` into this turn's cost.
//
// A total lower than the previous one means the CLI process started over (a
// resume in the same harness process), so its whole total is this turn's. A zero
// total is a turn the CLI did not price, and reports no cost.
func (a *UsageAggregator) turnCost(cumulativeUSD float64) *msg.Cost {
	if cumulativeUSD <= 0 {
		return nil
	}
	turnUSD := cumulativeUSD - a.cumulativeCostUSD
	if cumulativeUSD < a.cumulativeCostUSD {
		turnUSD = cumulativeUSD
	}
	a.cumulativeCostUSD = cumulativeUSD
	if turnUSD <= 0 {
		return nil
	}
	return &msg.Cost{TotalUSD: turnUSD}
}

// APICallUsages returns the per-call breakdown.
func (a *UsageAggregator) APICallUsages() []msg.TokenUsage {
	return a.calls
}

// ToolCalls returns the number of tool calls observed.
func (a *UsageAggregator) ToolCalls() int {
	return a.toolCalls
}

// Reset clears the aggregator for a new turn.
func (a *UsageAggregator) Reset() {
	a.calls = a.calls[:0]
	a.toolCalls = 0
}
