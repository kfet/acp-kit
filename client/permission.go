package client

import (
	"context"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// PermissionFunc adapts a function into a PermissionPolicy.
type PermissionFunc func(context.Context, acp.RequestPermissionRequest) acp.RequestPermissionResponse

// Decide implements PermissionPolicy.
func (f PermissionFunc) Decide(ctx context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return f(ctx, req)
}

// AllowAllPermissions approves a request by selecting an allow-shaped option,
// falling back to the first option when no option advertises allow/approve.
func AllowAllPermissions(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return pickPermission(req, "allow")
}

// DenyAllPermissions rejects a request by selecting a reject-shaped option,
// falling back to the first option when no option advertises reject/deny.
func DenyAllPermissions(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return pickPermission(req, "reject")
}

// ReadOnlyPermissions allows read-like tool calls and rejects everything else.
// Heuristic: if the tool call title contains a write/exec-shaped verb
// (write, edit, bash, exec, run, delete, rm), the request is rejected;
// otherwise it is allowed.
func ReadOnlyPermissions(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	title := ""
	if req.ToolCall.Title != nil {
		title = strings.ToLower(*req.ToolCall.Title)
	}
	for _, w := range []string{"write", "edit", "bash", "exec", "run", "delete", "rm "} {
		if strings.Contains(title, w) {
			return pickPermission(req, "reject")
		}
	}
	return pickPermission(req, "allow")
}

func pickPermission(req acp.RequestPermissionRequest, want string) acp.RequestPermissionResponse {
	var chosen acp.PermissionOptionId
	for _, o := range req.Options {
		n := strings.ToLower(o.Name)
		k := strings.ToLower(string(o.Kind))
		if strings.Contains(n, want) || strings.Contains(n, "approve") || strings.Contains(k, want) || strings.Contains(k, "approve") {
			chosen = o.OptionId
			break
		}
	}
	if chosen == "" && len(req.Options) > 0 {
		chosen = req.Options[0].OptionId
	}
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: chosen},
		},
	}
}
