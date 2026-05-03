package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

const version = "0.1.0"

// emitMu guards stdout writes so concurrent goroutines don't interleave JSON lines.
var emitMu sync.Mutex

// execClaudePTY replaces this process with the upstream `claude` CLI so
// the inherited pseudoterminal is wired straight to its native TUI. The
// caller (llm-bridge-server.StartProcessPTY) already set ANTHROPIC_API_KEY
// (or whichever credential the harness's auth path provides), and the cwd
// is honored from the parent. We deliberately pass no flags: no
// --input-format, no --output-format — the user wants the unmodified
// claude experience.
func execClaudePTY() {
	cfg := loadConfig()
	bin, err := exec.LookPath(cfg.ClaudePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-claudecode pty: claude binary not found at %q: %v\n", cfg.ClaudePath, err)
		os.Exit(127)
	}
	if err := syscall.Exec(bin, []string{bin}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-claudecode pty: exec %s: %v\n", bin, err)
		os.Exit(127)
	}
}

// emitEvent writes a canonical msg.Event as a single NDJSON line to stdout.
func emitEvent(ev any) {
	emitMu.Lock()
	defer emitMu.Unlock()

	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("failed to marshal event: %v", err)
		return
	}
	data = append(data, '\n')
	os.Stdout.Write(data)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version)
		os.Exit(0)
	}

	// PTY mode hand-off. llm-bridge-server's StartProcessPTY launches us
	// inside a pseudoterminal with LLMBRIDGE_PTY_MODE=1; the contract is
	// that we exec into the upstream `claude` CLI so the pty fd connects
	// directly to its TUI. The harness wrapper has nothing to do in pty
	// mode — there's no msg.Event translation, no JSON-RPC channel.
	if os.Getenv("LLMBRIDGE_PTY_MODE") == "1" {
		execClaudePTY()
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "-discover" {
		project := ""
		if len(os.Args) > 2 {
			project = os.Args[2]
		}
		sessions, err := discoverSessions(project)
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(sessions)
		os.Exit(0)
	}

	if len(os.Args) > 2 && os.Args[1] == "-import-history" {
		sessionID := os.Args[2]
		path := ""
		if len(os.Args) > 3 {
			path = os.Args[3]
		} else {
			// Resolve sessionID against state.db first: it may be a
			// bridge_session_id (look up its latest rollout) or a harness
			// UUID that was cold-imported as a synthetic single-rollout
			// session (bridge_session_id == harness UUID).
			if st, err := OpenState(DefaultStatePath()); err == nil {
				if rs, err := st.ListRollouts(sessionID); err == nil && len(rs) > 0 {
					path = rs[len(rs)-1].RolloutPath
					if path == "" {
						path = findRolloutForUUID(rs[len(rs)-1].HarnessSessionID)
					}
				}
				st.Close()
			}
			// Fall through to a direct filesystem walk for harness UUIDs
			// that aren't in state.db at all (legacy CC sessions, or
			// brand-new files not yet cold-imported).
			if path == "" {
				path = findRolloutForUUID(sessionID)
			}
		}
		if path == "" {
			// Idempotent no-op: a session id that resolves to no rollout on
			// disk emits no events and exits 0. Mirrors -discover returning
			// an empty array, and matches the conformance import contract
			// (exit 0 = handled, exit 2 = unimplemented).
			fmt.Fprintf(os.Stderr, "import-history: session not found: %s (no-op)\n", sessionID)
			os.Exit(0)
		}
		if err := importHistory(sessionID, path); err != nil {
			fmt.Fprintf(os.Stderr, "import-history: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	log.SetOutput(os.Stderr)
	log.SetPrefix("[llm-bridge-claudecode] ")

	cfg := loadConfig()
	h := NewHarness(cfg)

	// Open state.db once and orphan any pending WAL rows from a prior crash
	// before the JSON-RPC read loop begins. Failure here is fatal — without
	// state.db the chain contract silently regresses to in-memory IDs.
	if err := h.openStateAndRecover(); err != nil {
		log.Fatalf("open state.db: %v", err)
	}

	// Handle signals for graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received %v, shutting down", sig)
		if sig == syscall.SIGINT {
			h.Interrupt()
		} else {
			h.Shutdown()
			os.Exit(0)
		}
	}()

	// Read JSON-RPC requests from llm-bridge on stdin.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid request: %v (line: %s)", err, string(line))
			continue
		}

		log.Printf("request: method=%s", req.Method)
		if err := h.HandleRequest(req); err != nil {
			log.Printf("handler error: method=%s err=%v", req.Method, err)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stdin scanner error: %v", err)
	}

	// stdin closed — llm-bridge is done with us.
	log.Printf("stdin closed, shutting down")
	h.Shutdown()
}
