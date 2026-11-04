package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/kfet/acp-kit/client"
)

// `!mcp login <server>` — authorising a remote MCP server from chat.
//
// Plain passthrough to the agent's `/mcp login` cannot finish from a
// chat relay: the OAuth redirect lands on a loopback listener on the
// AGENT host, which the user's browser is usually not on, and nothing
// carries the landing URL back. So the broker drives the same two-call
// interactive authenticate protocol `!login` uses, with methodId
// "mcp:<server>" and the conversation's ACP session id in
// _meta.auth.sessionId (MCP servers are per session):
//
//	call 1  {interactive, sessionId}           → needs_redirect {id, url} | ok
//	call 2  {interactive, sessionId, id, redirect} → ok
//	cancel  {interactive, sessionId, id, cancel}
//
// The agent keeps its loopback listener running in parallel, so a
// browser that IS on the agent host still finishes on its own; the
// pasted URL is for the browser that is not.
//
// Everything else under `!mcp` (logout, reload, a bare `!mcp`) stays a
// passthrough. An agent too old to know "mcp:*" ids (see
// client.IsUnknownAuthMethod) gets the old passthrough too: Handle
// returns nil and the relay forwards `/mcp login <server>`.

// SessionAuthenticator is the OPTIONAL Authenticator capability behind
// `!mcp login`: authenticate within one ACP session.
// *client.AgentProc implements it.
type SessionAuthenticator interface {
	AuthenticateSession(ctx context.Context, sessionID, methodID, id, redirect string, cancel bool) (client.AuthResult, error)
}

// SessionLocator is the OPTIONAL Controller capability behind `!mcp
// login`: the conversation's live ACP session id, without creating one.
// acp-kit/convo's Controller implements it.
//
// It is deliberately a lookup and not a get-or-create: opening a
// session outside a turn would register a sink as the side effect of a
// command. With no live session the broker asks the user to send a
// message first.
type SessionLocator interface {
	ACPSessionID(convID string) (sessionID string, ok bool)
}

// MCPMethodPrefix prefixes an MCP server name to form its authenticate
// methodId ("mcp:github"). Such ids are dynamic: the agent never lists
// them in initialize, and the broker never checks them against
// AuthMethods.
const MCPMethodPrefix = "mcp:"

// mcpLoginAvail reports whether this broker can drive `!mcp login`
// itself. Without both capabilities the command stays a passthrough.
func (b *Broker) mcpLoginAvail() bool {
	if _, ok := b.a.(SessionAuthenticator); !ok {
		return false
	}
	_, ok := b.locator()
	return ok
}

// locator returns the Controller's SessionLocator capability, if any.
func (b *Broker) locator() (SessionLocator, bool) {
	if b.ctrl == nil {
		return nil, false
	}
	l, ok := b.ctrl.(SessionLocator)
	return l, ok
}

// mcpLoginArgs splits a sigil-stripped, verb-folded body of the form
// "mcp login <arg>" and returns the trimmed arg. The `login` word is
// case-insensitive like a verb (phones capitalise); the arg is kept as
// typed, because server names are literals.
func mcpLoginArgs(body string) (arg string, ok bool) {
	rest, ok := strings.CutPrefix(body, "mcp ")
	if !ok {
		return "", false
	}
	sub, arg, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if !strings.EqualFold(sub, "login") {
		return "", false
	}
	return strings.TrimSpace(arg), true
}

// isMCPCancelBody reports whether body is `mcp login cancel`.
func isMCPCancelBody(body string) bool {
	arg, ok := mcpLoginArgs(body)
	return ok && strings.EqualFold(arg, "cancel")
}

// isMCPLoginBody reports whether body is `mcp login <server>` (or
// `mcp login cancel`) AND this broker drives it. A bare `mcp login`
// is left to the agent, which lists its servers.
func (b *Broker) isMCPLoginBody(body string) bool {
	arg, ok := mcpLoginArgs(body)
	return ok && arg != "" && b.mcpLoginAvail()
}

// mcpServerArg returns the server name of an `mcp login <server>` body.
func mcpServerArg(body string) string {
	arg, _ := mcpLoginArgs(body)
	return arg
}

// startMCP runs call 1 of an MCP server login. Returns (nil, nil) — not
// handled, forward as passthrough — when the agent does not know
// "mcp:*" methods.
func (b *Broker) startMCP(ctx context.Context, convID, server string) (*Outcome, error) {
	b.mu.Lock()
	if existing, ok := b.pending[convID]; ok {
		b.mu.Unlock()
		return &Outcome{Text: alreadyPending(existing)}, nil
	}
	b.mu.Unlock()

	loc, _ := b.locator()
	sid, ok := loc.ACPSessionID(convID)
	if !ok || sid == "" {
		return &Outcome{Text: fmt.Sprintf("There is no agent session in this conversation yet, and MCP servers are connected per session. Send any message first so one starts, then send `%smcp login %s` again.", DisplaySigil, server)}, nil
	}

	methodID := MCPMethodPrefix + server
	res, err := mustSessionAuth(b.a).AuthenticateSession(ctx, sid, methodID, "", "", false)
	if client.IsUnknownAuthMethod(err) {
		return nil, nil
	}
	if err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ Couldn't start login for MCP server `%s`: %v", server, err)}, nil
	}
	switch res.State {
	case "needs_redirect":
		b.mu.Lock()
		b.pending[convID] = pendingEntry{methodID: methodID, authID: res.ID, sessionID: sid, mcpServer: server}
		b.mu.Unlock()
		text := fmt.Sprintf("Open this URL and authorise access to the `%s` MCP server:\n\n%s\n\n"+
			"Then paste here the URL of the page you land on — even if that page fails to load.\n", server, res.URL)
		if res.Instructions != "" {
			text += "\n" + res.Instructions + "\n"
		}
		text += fmt.Sprintf("\nSend `%smcp login cancel` to abort.\n", DisplaySigil)
		return &Outcome{Text: text, URL: res.URL, Instructions: res.Instructions}, nil
	case "ok", "":
		return &Outcome{Text: mcpAuthorised(server)}, nil
	case "cancelled":
		return &Outcome{Text: "Login cancelled."}, nil
	default:
		return &Outcome{Text: fmt.Sprintf("Login returned unexpected state: %q.", res.State)}, nil
	}
}

// mcpAuthorised is the success reply for an MCP server login.
func mcpAuthorised(server string) string {
	return fmt.Sprintf("✅ MCP server `%s` is authorised — its tools are available in this session.", server)
}

// cancelHint is the command that aborts this login.
func (e pendingEntry) cancelHint() string {
	if e.mcpServer != "" {
		return DisplaySigil + "mcp login cancel"
	}
	return DisplaySigil + "login cancel"
}

// alreadyPending refuses a second concurrent login for a conversation.
func alreadyPending(e pendingEntry) string {
	return fmt.Sprintf("A login is already in progress (%s). Paste the redirect URL or send `%s`.", e.methodID, e.cancelHint())
}
