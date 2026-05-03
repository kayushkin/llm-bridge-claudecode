package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestTranslateUserContentBlocks_TextOnly(t *testing.T) {
	got, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockText, Text: &msg.TextBlock{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Type != "text" || got[0].Text != "hello" {
		t.Errorf("got %+v, want {Type:text Text:hello}", got[0])
	}
	if got[0].Source != nil {
		t.Errorf("Source = %+v, want nil for text block", got[0].Source)
	}
}

func TestTranslateUserContentBlocks_ImageBase64(t *testing.T) {
	got, err := translateUserContentBlocks([]msg.ContentBlock{
		{
			Type: msg.BlockImage,
			Image: &msg.ImageBlock{Source: msg.MediaSource{
				Kind: msg.MediaBase64, MediaType: "image/png", Data: "AAAA",
			}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Type != "image" {
		t.Fatalf("got %+v, want type=image", got)
	}
	src := got[0].Source
	if src == nil {
		t.Fatal("Source nil, want non-nil for image block")
	}
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "AAAA" {
		t.Errorf("source = %+v, want {Type:base64 MediaType:image/png Data:AAAA}", src)
	}
	if src.URL != "" {
		t.Errorf("URL should be empty for base64 source, got %q", src.URL)
	}
}

func TestTranslateUserContentBlocks_ImageURL(t *testing.T) {
	got, err := translateUserContentBlocks([]msg.ContentBlock{
		{
			Type: msg.BlockImage,
			Image: &msg.ImageBlock{Source: msg.MediaSource{
				Kind: msg.MediaURL, Data: "https://example.com/cat.png",
			}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	src := got[0].Source
	if src == nil || src.Type != "url" || src.URL != "https://example.com/cat.png" {
		t.Errorf("source = %+v, want {Type:url URL:https://example.com/cat.png}", src)
	}
	if src.Data != "" {
		t.Errorf("Data should be empty for url source, got %q", src.Data)
	}
}

func TestTranslateUserContentBlocks_DocumentBase64(t *testing.T) {
	got, err := translateUserContentBlocks([]msg.ContentBlock{
		{
			Type: msg.BlockDocument,
			Document: &msg.DocumentBlock{Source: msg.MediaSource{
				Kind: msg.MediaBase64, MediaType: "application/pdf", Data: "JVBE",
			}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Type != "document" {
		t.Errorf("type = %q, want document", got[0].Type)
	}
	if got[0].Source.MediaType != "application/pdf" {
		t.Errorf("media_type = %q, want application/pdf", got[0].Source.MediaType)
	}
}

func TestTranslateUserContentBlocks_MixedTextAndImage(t *testing.T) {
	got, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockText, Text: &msg.TextBlock{Text: "describe this:"}},
		{
			Type: msg.BlockImage,
			Image: &msg.ImageBlock{Source: msg.MediaSource{
				Kind: msg.MediaBase64, MediaType: "image/jpeg", Data: "/9j/",
			}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Type != "text" || got[1].Type != "image" {
		t.Errorf("ordered types = %s,%s; want text,image", got[0].Type, got[1].Type)
	}
}

func TestTranslateUserContentBlocks_RejectToolUse(t *testing.T) {
	_, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockToolUse, ToolUse: &msg.ToolUseBlock{ID: "x", Name: "y"}},
	})
	if err == nil {
		t.Fatal("expected error for tool_use block in user input, got nil")
	}
	if !strings.Contains(err.Error(), "not valid in user input") {
		t.Errorf("err = %v, want mention of invalid user-input type", err)
	}
}

func TestTranslateUserContentBlocks_RejectFileID(t *testing.T) {
	_, err := translateUserContentBlocks([]msg.ContentBlock{
		{
			Type: msg.BlockImage,
			Image: &msg.ImageBlock{Source: msg.MediaSource{
				Kind: msg.MediaFileID, Data: "file_abc",
			}},
		},
	})
	if err == nil {
		t.Fatal("expected error for file_id source, got nil")
	}
	if !strings.Contains(err.Error(), "file_id") {
		t.Errorf("err = %v, want mention of file_id", err)
	}
}

func TestTranslateUserContentBlocks_RejectInvalid(t *testing.T) {
	// Block with Type=text but no Text payload — caught by Validate().
	_, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockText},
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

// TestUserMessageWireShape_Image golden-tests the full ccUserMessage JSON
// envelope that lands on CC's stdin for a text+image message, ensuring the
// shape matches Anthropic's content-block format CC expects.
func TestUserMessageWireShape_Image(t *testing.T) {
	wire, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockText, Text: &msg.TextBlock{Text: "what's in this?"}},
		{
			Type: msg.BlockImage,
			Image: &msg.ImageBlock{Source: msg.MediaSource{
				Kind: msg.MediaBase64, MediaType: "image/png", Data: "AAAA",
			}},
		},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	envelope := ccUserMessage{
		Type: "user",
		Message: ccMessage{
			Role:    "user",
			Content: wire,
		},
	}
	got, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"what's in this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},"session_id":"","parent_tool_use_id":null}`
	if string(got) != want {
		t.Errorf("wire mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestUserMessageWireShape_TextOnlyUnchanged confirms the legacy text-only
// path still emits the exact same JSON it did before the widening — i.e. no
// stray "source" field, no behavioral regression.
func TestUserMessageWireShape_TextOnlyUnchanged(t *testing.T) {
	wire, err := translateUserContentBlocks([]msg.ContentBlock{
		{Type: msg.BlockText, Text: &msg.TextBlock{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	envelope := ccUserMessage{
		Type:    "user",
		Message: ccMessage{Role: "user", Content: wire},
	}
	got, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"session_id":"","parent_tool_use_id":null}`
	if string(got) != want {
		t.Errorf("wire mismatch:\n got: %s\nwant: %s", got, want)
	}
}
