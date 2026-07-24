package main

import (
	"os"
	"strconv"
	"time"
)

// defaultTurnIdleTimeout is how long a turn may produce zero harness activity
// (no stream-json event, no OTel event) while the CC process is still alive
// before drainUntilResult treats it as wedged and unblocks. It must sit
// comfortably above the longest legitimate silence — a single overloaded model
// call can stall for ~3 minutes before its retry telemetry lands — so 5 minutes
// leaves headroom while still bounding a hang that would otherwise be infinite.
const defaultTurnIdleTimeout = 5 * time.Minute

// Config holds environment-based configuration for the harness.
type Config struct {
	ClaudePath string
	Model      string
	WorkDir    string
	APIKey     string

	// TurnIdleTimeout bounds how long drainUntilResult waits on a silent-but-
	// alive turn before surfacing a TURN_IDLE_TIMEOUT error and killing the
	// wedged process. Zero disables the watchdog (the turn waits forever on a
	// stream-json result, the pre-watchdog behavior).
	TurnIdleTimeout time.Duration
}

func loadConfig() *Config {
	return &Config{
		ClaudePath:      envOr("CLAUDE_PATH", "claude"),
		Model:           os.Getenv("CLAUDE_MODEL"),
		WorkDir:         os.Getenv("CLAUDE_WORKDIR"),
		APIKey:          os.Getenv("ANTHROPIC_API_KEY"),
		TurnIdleTimeout: turnIdleTimeoutFromEnv(),
	}
}

// turnIdleTimeoutFromEnv reads CLAUDECODE_TURN_IDLE_TIMEOUT_SEC. Unset uses the
// default; an explicit 0 disables the watchdog; a negative or unparseable value
// falls back to the default rather than silently disabling a safety net.
func turnIdleTimeoutFromEnv() time.Duration {
	raw := os.Getenv("CLAUDECODE_TURN_IDLE_TIMEOUT_SEC")
	if raw == "" {
		return defaultTurnIdleTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return defaultTurnIdleTimeout
	}
	if secs == 0 {
		return 0 // explicit opt-out
	}
	if secs < 0 {
		return defaultTurnIdleTimeout
	}
	return time.Duration(secs) * time.Second
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
