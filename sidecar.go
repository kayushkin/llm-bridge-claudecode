package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// runOTelSidecar is the entry point for `llm-bridge-claudecode -otel-sidecar`.
//
// Used by PTY-mode sessions, where the harness exec's directly into claude
// and leaves no Go process to host an in-process OTLP receiver. bridge-server
// spawns this sidecar before launching the PTY child, then injects
// OTEL_EXPORTER_OTLP_ENDPOINT into the child's environment pointing at the
// receiver URL the sidecar prints to stdout.
//
// Stdin is the lifecycle signal: when bridge-server closes the pipe (because
// the PTY child exited and bridge-server is tearing down the session), the
// sidecar drains pending OTel batches for a short flush window and exits.
//
// Required env:
//
//	LLMBRIDGE_BRIDGE_SESSION_ID  — the canonical bridge_session_id to stamp on emitted events
//	LLMBRIDGE_BRIDGE_SERVER_URL  — base URL bridge-server listens on; the sidecar
//	                               POSTs translated events to <url>/sidecar/event/<bridge_id>
//
// Output:
//   - stdout line 1: the receiver URL bridge-server should put in
//     OTEL_EXPORTER_OTLP_ENDPOINT on the PTY child. Followed by "\n".
//     bridge-server must read this line before launching the PTY so the
//     child sees the right endpoint from byte zero.
//   - stderr: human-readable status / errors. No structured output.
func runOTelSidecar() {
	bridgeSessionID := os.Getenv("LLMBRIDGE_BRIDGE_SESSION_ID")
	if bridgeSessionID == "" {
		fmt.Fprintln(os.Stderr, "otel-sidecar: LLMBRIDGE_BRIDGE_SESSION_ID is required")
		os.Exit(2)
	}
	bridgeServerURL := os.Getenv("LLMBRIDGE_BRIDGE_SERVER_URL")
	if bridgeServerURL == "" {
		fmt.Fprintln(os.Stderr, "otel-sidecar: LLMBRIDGE_BRIDGE_SERVER_URL is required")
		os.Exit(2)
	}

	emit := newSidecarEmitter(bridgeServerURL, bridgeSessionID)

	recv, err := NewOTelReceiver(emit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otel-sidecar: start receiver: %v\n", err)
		os.Exit(1)
	}
	recv.Start()

	// Hand the endpoint URL to bridge-server before doing anything else.
	// bridge-server blocks on this read, so any delay here delays the PTY
	// launch the user is waiting on. Flush immediately.
	fmt.Println(recv.EndpointURL())
	_ = os.Stdout.Sync()

	// Lifecycle: exit on either stdin close (parent decided session is done)
	// or SIGTERM (parent killed us). Either way we drain trailing OTel
	// batches with a short flush window before tearing down the receiver.
	stdinClosed := make(chan struct{})
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				close(stdinClosed)
				return
			}
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-stdinClosed:
	case sig := <-sigs:
		fmt.Fprintf(os.Stderr, "otel-sidecar: got signal %v, draining\n", sig)
	}

	// OTel exporter batches with the configured interval (1s in our env).
	// Wait ~2x that so trailing batches land before we close the listener.
	time.Sleep(2 * time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := recv.Stop(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "otel-sidecar: shutdown: %v\n", err)
	}
}

// newSidecarEmitter returns an emit callback that POSTs each translated
// msg.Event to bridge-server's /sidecar/event/{bridge_id} endpoint with
// the canonical bridge_session_id stamped on the event body.
//
// Failures are logged loudly (fail-fast-and-loud per CLAUDE.md) but don't
// abort the sidecar — losing one event is better than dropping all
// subsequent ones because of a transient HTTP hiccup.
func newSidecarEmitter(bridgeServerURL, bridgeSessionID string) func(msg.Event) {
	endpoint := fmt.Sprintf("%s/sidecar/event/%s", bridgeServerURL, bridgeSessionID)
	client := &http.Client{Timeout: 5 * time.Second}

	return func(ev msg.Event) {
		if ev.BridgeSessionID == "" {
			ev.BridgeSessionID = bridgeSessionID
		}
		// Harness identity helps the bridge-server's contract checks; OTel
		// payloads aren't strictly from "claude_code" the harness, but
		// they're observability for one, so stamp that for consistency
		// with -p mode events from llm-bridge-claudecode.
		if ev.Harness == "" {
			ev.Harness = msg.HarnessClaudeCode
		}

		body, err := json.Marshal(ev)
		if err != nil {
			log.Printf("[otel-sidecar] marshal event: %v", err)
			return
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			log.Printf("[otel-sidecar] build request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[otel-sidecar] post event: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[otel-sidecar] post event: status=%d", resp.StatusCode)
		}
	}
}
