package client

import (
	"context"
	"encoding/json"
	"fmt"

	acp "github.com/coder/acp-go-sdk"
)

// ExtMethod sends an agent extension request (a method outside the ACP
// schema, e.g. "_fir/mcp/tools/call") for session sid and returns the raw
// result. params must not hold "sessionId": ExtMethod sets it to the wire
// id of sid, so a call still reaches the right session after a respawn.
func (a *AgentProc) ExtMethod(ctx context.Context, sid acp.SessionId, method string, params map[string]any) (json.RawMessage, error) {
	if _, ok := params["sessionId"]; ok {
		return nil, fmt.Errorf("acp-kit: ExtMethod %s: params must not set sessionId", method)
	}
	g, wire, err := a.route(ctx, sid)
	if err != nil {
		return nil, err
	}
	body := make(map[string]any, len(params)+1)
	for k, v := range params {
		body[k] = v
	}
	body["sessionId"] = wire
	return rpc[json.RawMessage](ctx, g, method, body)
}
