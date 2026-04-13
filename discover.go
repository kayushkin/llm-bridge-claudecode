package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// discoverSessions scans Claude Code's on-disk session storage and returns
// all sessions found for the given project directory (or all projects if empty).
func discoverSessions(projectDir string) ([]msg.StoredSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	projectsDir := filepath.Join(home, ".claude", "projects")

	var dirs []string
	if projectDir != "" {
		// Convert path to CC's directory name format: /home/user/repos → -home-user-repos
		ccDirName := pathToCCProject(projectDir)
		dirs = append(dirs, filepath.Join(projectsDir, ccDirName))
	} else {
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(projectsDir, e.Name()))
			}
		}
	}

	var sessions []msg.StoredSession
	for _, dir := range dirs {
		project := ccProjectToPath(filepath.Base(dir))
		found, err := scanProjectDir(dir, project)
		if err != nil {
			continue
		}
		sessions = append(sessions, found...)
	}

	return sessions, nil
}

// scanProjectDir scans a single CC project directory for session .jsonl files.
func scanProjectDir(dir, project string) ([]msg.StoredSession, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var sessions []msg.StoredSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}

		sessionID := strings.TrimSuffix(e.Name(), ".jsonl")
		path := filepath.Join(dir, e.Name())

		info, err := e.Info()
		if err != nil {
			continue
		}

		sess := msg.StoredSession{
			ID:        sessionID,
			Harness:   msg.HarnessClaudeCode,
			Project:   project,
			UpdatedAt: info.ModTime(),
			Path:      path,
		}

		// Parse first line for metadata.
		if prompt, ts, turns := parseSessionHead(path); prompt != "" {
			sess.Prompt = prompt
			if !ts.IsZero() {
				sess.CreatedAt = ts
			}
			sess.TurnCount = turns
		}

		// Fall back to file mod time if no timestamp parsed.
		if sess.CreatedAt.IsZero() {
			sess.CreatedAt = info.ModTime()
		}

		sessions = append(sessions, sess)
	}

	return sessions, nil
}

// parseSessionHead reads the first line of a CC session JSONL file to extract
// the initial prompt and timestamp.
// It also does a fast line count of "type":"user" lines to approximate turn count.
func parseSessionHead(path string) (prompt string, ts time.Time, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	firstDone := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if !firstDone {
			firstDone = true
			var head struct {
				Type      string `json:"type"`
				Content   string `json:"content"`
				Timestamp string `json:"timestamp"`
			}
			if json.Unmarshal(line, &head) == nil {
				if head.Content != "" {
					prompt = truncate(head.Content, 200)
				}
				if head.Timestamp != "" {
					ts, _ = time.Parse(time.RFC3339Nano, head.Timestamp)
				}
			}
		}

		// Count user messages for turn count approximation.
		if json.Valid(line) {
			var entry struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.Type == "user" {
				turns++
			}
		}
	}

	return prompt, ts, turns
}

// pathToCCProject converts a filesystem path to Claude Code's project directory name.
// /home/user/repos → -home-user-repos
func pathToCCProject(path string) string {
	return strings.ReplaceAll(path, "/", "-")
}

// ccProjectToPath converts a CC project directory name back to a filesystem path.
// -home-user-repos → /home/user/repos
func ccProjectToPath(name string) string {
	if name == "" || name == "-" {
		return "/"
	}
	// CC format: leading dash is the root /, subsequent dashes are path separators
	return "/" + strings.ReplaceAll(strings.TrimPrefix(name, "-"), "-", "/")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
