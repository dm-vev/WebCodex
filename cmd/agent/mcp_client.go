package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// initializeRequest stays on one line because Codex MCP uses newline-delimited JSON-RPC over stdio.
const initializeRequest = `{"jsonrpc":"2.0","id":"webcodex-init",` +
	`"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
	`"capabilities":{},"clientInfo":{"name":"webcodex-agent","version":"0.1.0"}}}`

type jsonrpcMessage struct {
	ID json.RawMessage `json:"id,omitempty"`
}

// mcpClient serializes writes to Codex MCP and dispatches responses by JSON-RPC ID.
type mcpClient struct {
	stdin io.WriteCloser
	mu    sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage
}

func (c *mcpClient) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	response, err := c.call(ctx, json.RawMessage(initializeRequest))
	if err != nil {
		return fmt.Errorf("initialize Codex MCP: %w", err)
	}

	var msg struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response, &msg); err != nil {
		return fmt.Errorf("parse initialize response: %w", err)
	}
	if msg.Error != nil {
		return errors.New(msg.Error.Message)
	}
	if _, err := c.stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		return fmt.Errorf("write initialized notification: %w", err)
	}
	return nil
}

// startMCP launches the configured Codex MCP server over stdio.
func startMCP(ctx context.Context, shellCmd string) (*mcpClient, error) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", shellCmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	client := &mcpClient{
		stdin:   stdin,
		pending: make(map[string]chan json.RawMessage),
	}
	go client.readLoop(stdout)
	go logPipe("codex-mcp", stderr)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("codex mcp exited: %v", err)
		}
	}()
	return client, nil
}

func (c *mcpClient) call(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	var msg jsonrpcMessage
	if err := json.Unmarshal(request, &msg); err != nil {
		return nil, fmt.Errorf("parse jsonrpc request: %w", err)
	}

	key := string(msg.ID)
	if key == "" {
		return nil, c.write(request)
	}

	respCh := make(chan json.RawMessage, 1)
	c.pendingMu.Lock()
	c.pending[key] = respCh
	c.pendingMu.Unlock()
	defer c.forget(key)

	if err := c.write(request); err != nil {
		return nil, err
	}

	select {
	case response := <-respCh:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *mcpClient) write(request json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.stdin.Write(request); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	if _, err := c.stdin.Write([]byte("\n")); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// readLoop dispatches each Codex MCP response to the call waiting on its JSON-RPC ID.
func (c *mcpClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("codex mcp bad json: %v", err)
			continue
		}
		key := string(msg.ID)
		if key == "" {
			continue
		}

		c.pendingMu.Lock()
		respCh := c.pending[key]
		c.pendingMu.Unlock()
		if respCh == nil {
			log.Printf("codex mcp response for unknown id %s", key)
			continue
		}

		response := append(json.RawMessage(nil), line...)
		respCh <- response
	}
	if err := scanner.Err(); err != nil {
		log.Printf("codex mcp stdout: %v", err)
	}
}

func (c *mcpClient) forget(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

func logPipe(name string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		log.Printf("%s: %s", name, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("%s: %v", name, err)
	}
}
