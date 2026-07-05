package protocol

import "encoding/json"

// AgentRequest is one raw MCP JSON-RPC message sent from the gate to the local agent.
type AgentRequest struct {
	ID      string          `json:"id"`
	Request json.RawMessage `json:"request"`
}

// AgentResponse is one raw MCP JSON-RPC response sent from the local agent to the gate.
type AgentResponse struct {
	ID       string          `json:"id"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}
