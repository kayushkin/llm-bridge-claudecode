package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// permissionStoreClient evaluates a tool call by POSTing to the
// permission-store /evaluate endpoint. It implements PermissionMCP's
// Evaluator. Any error (network down, malformed response, non-2xx) collapses
// to ask with a human-readable message — never silently allow.
type permissionStoreClient struct {
	url             string
	bridgeIDOf      func() string
	instanceIDOf    func() string
	httpClient      *http.Client
}

func newPermissionStoreClient(bridgeIDOf, instanceIDOf func() string) *permissionStoreClient {
	url := os.Getenv("PERMISSION_STORE_URL")
	if url == "" {
		url = "http://localhost:8304"
	}
	return &permissionStoreClient{
		url:          url,
		bridgeIDOf:   bridgeIDOf,
		instanceIDOf: instanceIDOf,
		httpClient: &http.Client{
			// Loopback HTTP — the call should be sub-millisecond. The
			// timeout is a generous ceiling so a wedged store collapses to
			// ask quickly rather than holding CC's tool call forever.
			Timeout: 3 * time.Second,
		},
	}
}

func (c *permissionStoreClient) evaluate(toolName string, input json.RawMessage) EvaluateResult {
	body := struct {
		BridgeID   string          `json:"bridge_id,omitempty"`
		InstanceID string          `json:"instance_id,omitempty"`
		Tool       string          `json:"tool"`
		Input      json.RawMessage `json:"input,omitempty"`
	}{
		BridgeID:   c.bridgeIDOf(),
		InstanceID: c.instanceIDOf(),
		Tool:       toolName,
		Input:      input,
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return askOnError("marshal evaluate request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url+"/evaluate", bytes.NewReader(bodyJSON))
	if err != nil {
		return askOnError("build evaluate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return askOnError("permission-store unreachable: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return askOnError("read evaluate response: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return askOnError("permission-store HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		Outcome       string          `json:"outcome"`
		MatchedRuleID string          `json:"matched_rule_id"`
		Message       string          `json:"message"`
		// updated_input is not currently surfaced by permission-store, but
		// the engine's design admits it; pass through if present.
		UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return askOnError("decode evaluate response: %v", err)
	}
	switch parsed.Outcome {
	case "allow", "deny", "ask":
		return EvaluateResult{
			Outcome:       parsed.Outcome,
			Message:       parsed.Message,
			UpdatedInput:  parsed.UpdatedInput,
			MatchedRuleID: parsed.MatchedRuleID,
		}
	}
	return askOnError("permission-store returned unknown outcome %q", parsed.Outcome)
}

func askOnError(format string, args ...any) EvaluateResult {
	return EvaluateResult{
		Outcome: "ask",
		Message: fmt.Sprintf(format, args...),
	}
}
