package command

// mustSessionAuth returns a as a SessionAuthenticator.
//
// It panics if a is not one, which cannot happen: the only callers run
// for a login started by startMCP (directly, or via a pending entry with
// a sessionID), and startMCP is only reached when mcpLoginAvail has
// confirmed the capability. The Authenticator is fixed at New, so the
// answer cannot change in between.
func mustSessionAuth(a Authenticator) SessionAuthenticator {
	sa, ok := a.(SessionAuthenticator)
	if !ok {
		panic("command: mcp login without a SessionAuthenticator")
	}
	return sa
}
