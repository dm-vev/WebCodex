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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"webcodex/internal/protocol"
)

type jsonrpcMessage struct {
	ID json.RawMessage `json:"id,omitempty"`
}

type mcpClient struct {
	stdin io.WriteCloser
	mu    sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage
}

func main() {
	gateURL := strings.TrimRight(env("WEBCODEX_GATE_URL", ""), "/")
	token := env("WEBCODEX_AGENT_TOKEN", "")
	codexCmd := env("WEBCODEX_CODEX_MCP_CMD", "third_party/codex/codex-rs/target/debug/codex-mcp-server")
	if gateURL == "" || token == "" {
		log.Fatal("WEBCODEX_GATE_URL and WEBCODEX_AGENT_TOKEN are required")
	}

	mcp, err := startMCP(context.Background(), codexCmd)
	if err != nil {
		log.Fatalf("start codex mcp: %v", err)
	}
	if err := mcp.initialize(context.Background()); err != nil {
		log.Fatalf("initialize codex mcp: %v", err)
	}

	client := &http.Client{}
	for {
		if err := streamOnce(context.Background(), client, gateURL, token, mcp); err != nil {
			log.Printf("stream: %v", err)
			time.Sleep(time.Second)
		}
	}
}

func (c *mcpClient) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	response, err := c.call(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":"webcodex-init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"webcodex-agent","version":"0.1.0"}}}`))
	if err != nil {
		return err
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

func streamOnce(ctx context.Context, client *http.Client, gateURL, token string, mcp *mcpClient) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateURL+"/agent/stream", nil)
	if err != nil {
		return fmt.Errorf("create stream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream status %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var request protocol.AgentRequest
		if err := json.Unmarshal(line, &request); err != nil {
			log.Printf("bad stream json: %v", err)
			continue
		}
		go handleRequest(ctx, client, gateURL, token, mcp, request)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return errors.New("stream closed")
}

func handleRequest(
	ctx context.Context,
	client *http.Client,
	gateURL string,
	token string,
	mcp *mcpClient,
	request protocol.AgentRequest,
) {
	callCtx, cancel := context.WithTimeout(ctx, durationEnv("WEBCODEX_MCP_CALL_TIMEOUT", 2*time.Minute))
	defer cancel()

	response, err := mcp.call(callCtx, request.Request)
	result := protocol.AgentResponse{ID: request.ID, Response: response}
	if err != nil {
		result.Error = err.Error()
	}
	if request.ID == "" {
		return
	}
	if err := sendResult(ctx, client, gateURL, token, result); err != nil {
		log.Printf("send result: %v", err)
	}
}

func sendResult(
	ctx context.Context,
	client *http.Client,
	gateURL string,
	token string,
	result protocol.AgentResponse,
) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(result); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, gateURL+"/agent/result", &body)
	if err != nil {
		return fmt.Errorf("create result request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("result status %s", resp.Status)
	}
	return nil
}

func logPipe(name string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		log.Printf("%s: %s", name, scanner.Text())
	}
}

func env(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}
