package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"webcodex/internal/protocol"
)

const mcpProtocolVersion = "2025-06-18"

type jsonrpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

// handleMCP implements the MCP Streamable HTTP entry point and forwards non-local calls to the agent.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	authorized := bearerOK(r, s.publicToken)
	log.Printf(
		"mcp %s %s ua=%q auth=%t accept=%q content_type=%q session=%q protocol=%q",
		r.Method,
		r.URL.Path,
		r.UserAgent(),
		authorized,
		r.Header.Get("Accept"),
		r.Header.Get("Content-Type"),
		r.Header.Get("Mcp-Session-Id"),
		r.Header.Get("Mcp-Protocol-Version"),
	)
	w.Header().Set("Mcp-Protocol-Version", mcpProtocolVersion)
	if r.Method == http.MethodGet {
		s.handleMCPStream(w, r, authorized)
		return
	}
	if r.Method == http.MethodDelete {
		if !authorized {
			s.writeMCPUnauthorized(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized {
		s.writeMCPUnauthorized(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPCError(w, nil, -32700, "read request")
		return
	}
	var msg jsonrpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("mcp invalid json bytes=%d", len(body))
		writeRPCError(w, nil, -32700, "invalid json")
		return
	}
	log.Printf("mcp request method=%q id=%s bytes=%d", msg.Method, string(msg.ID), len(body))
	if msg.Method == "initialize" {
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result": map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{
					"resources": map[string]any{
						"listChanged": true,
					},
					"tools": map[string]any{
						"listChanged": true,
					},
				},
				"serverInfo": map[string]string{
					"name":    "webcodex",
					"title":   "WebCodex",
					"version": "0.1.0",
				},
			},
		})
		log.Printf("mcp response ok method=%q id=%s local=true elapsed=%s", msg.Method, string(msg.ID), time.Since(started))
		return
	}
	result, handled, err := localMCPResult(msg.Method, body, s.toolCards)
	if err != nil {
		log.Printf("mcp local response error method=%q id=%s error=%v", msg.Method, string(msg.ID), err)
		writeRPCError(w, msg.ID, -32000, err.Error())
		return
	}
	if handled {
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result":  result,
		})
		log.Printf("mcp response ok method=%q id=%s local=true elapsed=%s", msg.Method, string(msg.ID), time.Since(started))
		return
	}

	if len(msg.ID) == 0 {
		if err := s.enqueue(r.Context(), protocol.AgentRequest{ID: "", Request: body}); err != nil {
			log.Printf("mcp notification enqueue method=%q error=%v", msg.Method, err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("mcp notification accepted method=%q elapsed=%s", msg.Method, time.Since(started))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	toolName := ""
	toolArguments := map[string]any{}
	if msg.Method == "tools/call" {
		call, err := parseToolCall(body)
		if err != nil {
			writeRPCError(w, msg.ID, -32602, err.Error())
			return
		}
		if !s.toolPolicy.allows(call.Name) {
			writeRPCError(w, msg.ID, -32602, "tool not allowed")
			return
		}
		toolName = call.Name
		toolArguments = call.Arguments
	}

	resp, err := s.callAgent(r, body)
	if err != nil {
		log.Printf(
			"mcp response error method=%q id=%s error=%v elapsed=%s",
			msg.Method,
			string(msg.ID),
			err,
			time.Since(started),
		)
		writeRPCError(w, msg.ID, -32000, err.Error())
		return
	}
	if msg.Method == "tools/list" {
		response, err := s.filterToolsList(resp.Response)
		if err != nil {
			log.Printf(
				"mcp response error method=%q id=%s error=%v elapsed=%s",
				msg.Method,
				string(msg.ID),
				err,
				time.Since(started),
			)
			writeRPCError(w, msg.ID, -32000, err.Error())
			return
		}
		resp.Response = response
	}
	if msg.Method == "tools/call" {
		if s.toolCards {
			response, err := decorateToolCallResponse(resp.Response, toolName, toolArguments)
			if err != nil {
				log.Printf(
					"mcp response error method=%q id=%s error=%v elapsed=%s",
					msg.Method,
					string(msg.ID),
					err,
					time.Since(started),
				)
				writeRPCError(w, msg.ID, -32000, err.Error())
				return
			}
			resp.Response = response
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp.Response); err != nil {
		log.Printf("write mcp response: %v", err)
	}
	log.Printf(
		"mcp response ok method=%q id=%s bytes=%d elapsed=%s",
		msg.Method,
		string(msg.ID),
		len(resp.Response),
		time.Since(started),
	)
}

// handleMCPStream answers the optional MCP GET transport with an authenticated SSE readiness event.
func (s *server) handleMCPStream(w http.ResponseWriter, r *http.Request, authorized bool) {
	if !authorized {
		s.writeMCPUnauthorized(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := fmt.Fprint(w, ": webcodex stream ready\n\n"); err != nil {
		log.Printf("write mcp stream: %v", err)
	}
}

func (s *server) writeMCPUnauthorized(w http.ResponseWriter) {
	w.Header().Set(
		"WWW-Authenticate",
		fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.publicURL),
	)
}

type toolCall struct {
	Name      string
	Arguments map[string]any
}

func parseToolCall(request json.RawMessage) (toolCall, error) {
	var msg struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request, &msg); err != nil {
		return toolCall{}, fmt.Errorf("parse tool call: %w", err)
	}
	if msg.Params.Name == "" {
		return toolCall{}, errors.New("missing tool name")
	}
	if msg.Params.Arguments == nil {
		msg.Params.Arguments = map[string]any{}
	}
	return toolCall{Name: msg.Params.Name, Arguments: msg.Params.Arguments}, nil
}
