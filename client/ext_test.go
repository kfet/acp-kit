package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestExtMethod(t *testing.T) {
	var got map[string]any
	h := func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		case "_x/echo":
			_ = json.Unmarshal(params, &got)
			return map[string]any{"ok": true}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, h)
	ctx := context.Background()
	raw, err := pc.agent.ExtMethod(ctx, "s1", "_x/echo", map[string]any{"a": 1})
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	if got["sessionId"] != "s1" || got["a"] != float64(1) {
		t.Fatalf("params = %v", got)
	}
	if _, err := pc.agent.ExtMethod(ctx, "s1", "_x/none", nil); err == nil {
		t.Fatal("want method-not-found error")
	}
	if _, err := pc.agent.ExtMethod(ctx, "s1", "_x/echo", map[string]any{"sessionId": "z"}); err == nil || !strings.Contains(err.Error(), "sessionId") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtMethodClosedAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := supervised(t, ctx, nil)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExtMethod(ctx, "s1", "_x/echo", nil); err == nil {
		t.Fatal("want error on closed agent")
	}
}
