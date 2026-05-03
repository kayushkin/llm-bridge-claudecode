package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestStartParams_UnmarshalsBlocks verifies the JSON tag wiring on the new
// Blocks field — typos here would silently drop multimodal payloads.
func TestStartParams_UnmarshalsBlocks(t *testing.T) {
	raw := `{
		"display_name": "test",
		"agent_id": "a",
		"blocks": [
			{"type":"text","text_block":{"text":"describe:"}},
			{"type":"image","image_block":{"source":{"kind":"base64","media_type":"image/png","data":"AAAA"}}}
		]
	}`
	var p StartParams
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Blocks) != 2 {
		t.Fatalf("len(Blocks) = %d, want 2", len(p.Blocks))
	}
	if p.Blocks[0].Type != msg.BlockText || p.Blocks[0].Text == nil || p.Blocks[0].Text.Text != "describe:" {
		t.Errorf("block[0] = %+v, want text 'describe:'", p.Blocks[0])
	}
	if p.Blocks[1].Type != msg.BlockImage || p.Blocks[1].Image == nil {
		t.Fatalf("block[1] = %+v, want image", p.Blocks[1])
	}
	if p.Blocks[1].Image.Source.Kind != msg.MediaBase64 || p.Blocks[1].Image.Source.MediaType != "image/png" {
		t.Errorf("image source = %+v, want base64 image/png", p.Blocks[1].Image.Source)
	}
}

func TestMessageParams_UnmarshalsBlocks(t *testing.T) {
	raw := `{"blocks":[{"type":"text","text_block":{"text":"hi"}}]}`
	var p MessageParams
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != msg.BlockText {
		t.Fatalf("Blocks = %+v, want one text block", p.Blocks)
	}
	if p.Content != "" {
		t.Errorf("Content = %q, want empty when only blocks set", p.Content)
	}
}

// TestHandleMessage_RejectsBothFields confirms Content + Blocks is a hard
// error (fail fast and loud), not silent precedence. Uses a zero-value
// Harness — the conflict check is the first thing handleMessage does, so
// no other fields are touched before returning.
func TestHandleMessage_RejectsBothFields(t *testing.T) {
	h := &Harness{}
	err := h.handleMessage(MessageParams{
		Content: "hello",
		Blocks: []msg.ContentBlock{
			{Type: msg.BlockText, Text: &msg.TextBlock{Text: "hello"}},
		},
	})
	if err == nil {
		t.Fatal("expected error when both Content and Blocks set, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want 'mutually exclusive'", err)
	}
}

// TestHandleStart_RejectsBothFields confirms the same for the start path.
// The conflict check sits at function entry, so the validation runs before
// any subprocess spawn or state mutation.
func TestHandleStart_RejectsBothFields(t *testing.T) {
	h := &Harness{}
	err := h.handleStart(StartParams{
		Prompt: "hello",
		Blocks: []msg.ContentBlock{
			{Type: msg.BlockText, Text: &msg.TextBlock{Text: "hello"}},
		},
	})
	if err == nil {
		t.Fatal("expected error when both Prompt and Blocks set, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want 'mutually exclusive'", err)
	}
}
