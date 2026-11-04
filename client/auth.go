package client

import (
	"errors"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// AuthMethod describes one authentication method advertised by the agent in
// the initialize response. Extra _meta fields the client doesn't use are
// ignored.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "agent" | "env_var" | "terminal" | ""
}

// AuthResult is the outcome of an Authenticate call.
type AuthResult struct {
	// State is one of "needs_redirect", "ok", "cancelled", or "" if the
	// agent's response carried no _meta.auth state field.
	State string
	// ID is the opaque pending-login id the agent returns on call 1.
	ID string
	// URL is the auth URL the user should visit (state="needs_redirect").
	URL string
	// Instructions is optional human-readable text alongside URL.
	Instructions string
}

// IsUnknownAuthMethod reports whether err is an agent's refusal of an
// authenticate methodId it does not know — as opposed to a known method
// that failed. A relay uses it to detect an agent too old for a dynamic
// method such as "mcp:<server>" and fall back to whatever it did before.
//
// Two shapes count: a JSON-RPC "method not found" (-32601), and the
// "unknown auth method" text fir returns (as an internal error) for an
// id it does not advertise. Anything else — an unknown MCP server, a
// server that is not OAuth, a failed exchange — is a real answer from an
// agent that does support the method, and must not be papered over.
func IsUnknownAuthMethod(err error) bool {
	if err == nil {
		return false
	}
	var re *acp.RequestError
	if errors.As(err, &re) && re.Code == -32601 {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown auth method") ||
		strings.Contains(msg, "unknown authentication method") ||
		strings.Contains(msg, "method not found")
}
