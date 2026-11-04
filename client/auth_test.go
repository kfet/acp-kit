package client

import (
	"errors"
	"fmt"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestIsUnknownAuthMethod(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{acp.NewMethodNotFound("authenticate"), true},
		{fmt.Errorf("wrapped: %w", acp.NewMethodNotFound("authenticate")), true},
		{acp.NewInternalError(map[string]any{"error": "unknown auth method: mcp:gh"}), true},
		{errors.New("Unknown authentication method mcp:gh"), true},
		{errors.New("json-rpc: Method not found"), true},
		{acp.NewInternalError(map[string]any{"error": "unknown MCP server: gh"}), false},
		{acp.NewInvalidParams(map[string]any{"error": "sessionId required"}), false},
		{errors.New("token exchange failed"), false},
	}
	for _, c := range cases {
		if got := IsUnknownAuthMethod(c.err); got != c.want {
			t.Errorf("IsUnknownAuthMethod(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
