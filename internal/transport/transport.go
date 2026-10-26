package transport

import (
	"context"
	"encoding/json"
)

// Message represents a JSON-RPC message
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error represents a JSON-RPC error
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// CredentialRejectedError is answered with HTTP 401, the only response that
// makes an OAuth client sign in again; a JSON-RPC error never does.
type CredentialRejectedError struct {
	Err error
}

func (e *CredentialRejectedError) Error() string { return e.Err.Error() }

func (e *CredentialRejectedError) Unwrap() error { return e.Err }

// Transport defines the interface for MCP communication transports
type Transport interface {
	// Start initializes the transport and begins listening for messages
	Start(ctx context.Context) error

	// ReadMessage reads the next message from the transport
	ReadMessage() (*Message, error)

	// WriteMessage writes a message to the transport
	WriteMessage(msg *Message) error

	// Close gracefully shuts down the transport
	Close() error
}

// Handler processes incoming messages and returns responses
type Handler func(ctx context.Context, msg *Message) (*Message, error)
