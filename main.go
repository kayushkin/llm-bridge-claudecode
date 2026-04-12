package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const version = "0.1.0"

// emitMu guards stdout writes so concurrent goroutines don't interleave JSON lines.
var emitMu sync.Mutex

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

	log.SetOutput(os.Stderr)
	log.SetPrefix("[llm-bridge-claudecode] ")

	cfg := loadConfig()
	h := NewHarness(cfg)

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
