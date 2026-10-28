package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kayushkin/llm-bridge/msg"
)

// oneShotImageMediaTypes are the pictures Claude accepts.
var oneShotImageMediaTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// oneShotImageInput is the one user message `claude -p --input-format
// stream-json` reads: the images first, then the prompt, as content blocks.
// The plain-text stdin the CLI otherwise reads cannot carry an image.
func oneShotImageInput(request msg.OneShotRequest) ([]byte, error) {
	content := make([]map[string]any, 0, len(request.Images)+1)
	for index, image := range request.Images {
		if !slices.Contains(oneShotImageMediaTypes, image.MediaType) {
			return nil, fmt.Errorf("image %d: media_type %q is not one of %v", index, image.MediaType, oneShotImageMediaTypes)
		}
		if _, err := base64.StdEncoding.DecodeString(image.DataBase64); err != nil || image.DataBase64 == "" {
			return nil, fmt.Errorf("image %d: data_base64 is not base64", index)
		}
		content = append(content, map[string]any{"type": "image",
			"source": map[string]string{"type": "base64", "media_type": image.MediaType, "data": image.DataBase64}})
	}
	content = append(content, map[string]any{"type": "text", "text": request.Prompt})
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// oneShotStreamResult finds the result event in `--output-format
// stream-json` output: the same object `--output-format json` prints alone.
func oneShotStreamResult(output []byte) ([]byte, error) {
	var result []byte
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "result" {
			result = append([]byte(nil), scanner.Bytes()...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("the stream held no result event")
	}
	return result, nil
}
