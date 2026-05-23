// Package auth contains the small auth schema used by ACP authenticate extensions.
package auth

// Method describes one authentication method advertised by the agent in the
// initialize response. Extra _meta fields the relay doesn't use are ignored.
type Method struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "agent" | "env_var" | "terminal" | ""
}

// Result is the outcome of an Authenticate call.
type Result struct {
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
