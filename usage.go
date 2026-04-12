package main

import (
	"encoding/json"

	"github.com/kayushkin/llm-bridge/msg"
)

// UsageAggregator accumulates per-API-call token usage across a multi-turn run.
type UsageAggregator struct {
	calls     []msg.TokenUsage
	toolCalls int
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

	// Extract context window and total cost from modelUsage.
	for _, mu := range result.ModelUsage {
		usage.ContextLimit = mu.ContextWindow
		// Use the last model entry's context for ContextTokens estimate.
		usage.ContextTokens = mu.InputTokens + mu.CacheReadInputTokens + mu.CacheCreationInputTokens
	}

	var cost *msg.Cost
	if result.TotalCost > 0 {
		cost = &msg.Cost{TotalUSD: result.TotalCost}
	}

	return usage, cost
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
