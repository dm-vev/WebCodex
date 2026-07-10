package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"webcodex/internal/protocol"
)

type activeAgentStream struct {
	cancel context.CancelFunc
}

func (s *server) activateAgentStream(parent context.Context) (context.Context, *activeAgentStream, bool) {
	streamCtx, cancel := context.WithCancel(parent)
	stream := &activeAgentStream{cancel: cancel}

	s.mu.Lock()
	previous := s.stream
	s.stream = stream
	s.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}

	return streamCtx, stream, previous != nil
}

func (s *server) deactivateAgentStream(stream *activeAgentStream) {
	stream.cancel()
	s.mu.Lock()
	if s.stream == stream {
		s.stream = nil
	}
	s.mu.Unlock()
}

// Agent requests are correlated by random IDs because results arrive on a separate HTTP endpoint.
func (s *server) callAgent(r *http.Request, request json.RawMessage) (protocol.AgentResponse, error) {
	id, err := randomID()
	if err != nil {
		return protocol.AgentResponse{}, fmt.Errorf("create request id: %w", err)
	}

	resultCh := make(chan protocol.AgentResponse, 1)
	s.mu.Lock()
	s.pending[id] = resultCh
	s.mu.Unlock()
	defer s.forget(id)

	if err := s.enqueue(r.Context(), protocol.AgentRequest{ID: id, Request: request}); err != nil {
		return protocol.AgentResponse{}, err
	}

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		if result.Error != "" {
			return protocol.AgentResponse{}, errors.New(result.Error)
		}
		return result, nil
	case <-timer.C:
		return protocol.AgentResponse{}, errors.New("agent call timed out")
	case <-r.Context().Done():
		return protocol.AgentResponse{}, r.Context().Err()
	}
}

func (s *server) enqueue(ctx context.Context, request protocol.AgentRequest) error {
	select {
	case s.queue <- request:
		return nil
	case <-ctx.Done():
		return errors.New("request cancelled")
	default:
		return errors.New("agent queue is full or no agent is connected")
	}
}

// handleAgentStream sends queued requests over the agent's outbound NDJSON connection.
func (s *server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r, s.agentToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := fmt.Fprintln(w); err != nil {
		return
	}
	flusher.Flush()

	streamCtx, stream, replaced := s.activateAgentStream(r.Context())
	log.Printf("agent stream connected replaced=%t", replaced)
	defer func() {
		s.deactivateAgentStream(stream)
		log.Printf("agent stream disconnected")
	}()

	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case request := <-s.queue:
			log.Printf("agent stream dispatch id=%s bytes=%d", request.ID, len(request.Request))
			if err := enc.Encode(request); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintln(w); err != nil {
				return
			}
			flusher.Flush()
		case <-streamCtx.Done():
			return
		}
	}
}

// handleAgentResult completes the pending MCP call identified by the agent response ID.
func (s *server) handleAgentResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r, s.agentToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var result protocol.AgentResponse
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	resultCh := s.pending[result.ID]
	s.mu.Unlock()
	if resultCh == nil {
		http.Error(w, "unknown request id", http.StatusNotFound)
		return
	}

	resultCh <- result
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}
