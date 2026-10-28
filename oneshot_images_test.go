package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestImagesGoAheadOfThePromptInOneUserMessage(t *testing.T) {
	line, err := oneShotImageInput(msg.OneShotRequest{Prompt: "who is this?",
		Images: []msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: "AAEC"}}})
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type   string            `json:"type"`
				Text   string            `json:"text"`
				Source map[string]string `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatal(err)
	}
	content := message.Message.Content
	if message.Type != "user" || len(content) != 2 || content[0].Type != "image" || content[0].Source["data"] != "AAEC" || content[1].Text != "who is this?" {
		t.Errorf("message = %s", line)
	}
	if _, err := oneShotImageInput(msg.OneShotRequest{Prompt: "x", Images: []msg.OneShotImage{{MediaType: "image/tiff", DataBase64: "AAEC"}}}); err == nil {
		t.Error("a tiff was accepted")
	}
	if _, err := oneShotImageInput(msg.OneShotRequest{Prompt: "x", Images: []msg.OneShotImage{{MediaType: "image/png", DataBase64: "not base64!"}}}); err == nil {
		t.Error("bytes that are not base64 were accepted")
	}
}

func TestTheResultEventIsFoundInTheStream(t *testing.T) {
	stream := strings.Join([]string{`{"type":"system","subtype":"init"}`, `{"type":"assistant","message":{}}`,
		`{"type":"result","is_error":false,"result":"a crab","stop_reason":"end_turn"}`}, "\n")
	result, err := oneShotStreamResult([]byte(stream))
	if err != nil || !strings.Contains(string(result), `"a crab"`) {
		t.Errorf("result = %s, %v", result, err)
	}
	if _, err := oneShotStreamResult([]byte(`{"type":"system"}`)); err == nil {
		t.Error("a stream with no result was accepted")
	}
}
