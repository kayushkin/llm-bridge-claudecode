package main

import "os"

// Config holds environment-based configuration for the harness.
type Config struct {
	ClaudePath string
	Model      string
	WorkDir    string
	APIKey     string
}

func loadConfig() *Config {
	return &Config{
		ClaudePath: envOr("CLAUDE_PATH", "claude"),
		Model:      os.Getenv("CLAUDE_MODEL"),
		WorkDir:    os.Getenv("CLAUDE_WORKDIR"),
		APIKey:     os.Getenv("ANTHROPIC_API_KEY"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
