package command

import (
	"io/fs"
	"sort"
	"strings"
	"testing/fstest" // no dependency on package testing or flag
)

// SkillName is the name of the built-in relay-commands skill.
const SkillName = "relay-commands"

// PassthroughCommands returns the sorted passthrough allowlist: the
// agent commands a user may send as `!<cmd> args`, forwarded as
// `/<cmd> args`.
func PassthroughCommands() []string {
	out := make([]string, 0, len(passthroughAllow))
	for name := range passthroughAllow {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SkillMarkdown renders the relay-commands SKILL.md. It is GENERATED
// from brokerCommands and passthroughAllow so it cannot drift from the
// code that actually handles the commands.
func SkillMarkdown() string {
	s := DisplaySigil
	var b strings.Builder
	b.WriteString(`---
builtin: true
name: ` + SkillName + `
description: How the user drives this harness from chat with ` + "`" + s + "`" + ` commands. Load whenever you must tell the user which command to send — e.g. a tool or MCP server reports it needs auth/login, the user asks how to switch model, reset the session, reload, or compact.
---

# Relay chat commands

You are behind a chat relay. The user cannot type slash commands: chat
clients swallow a leading "/". Every command is sent as a chat message
starting with ` + "`" + s + "`" + ` (` + "`.`" + ` also works). When you tell the user what
to send, always write it with ` + "`" + s + "`" + `, never ` + "`/`" + `.

## Relay commands (handled by the relay itself)

Commands marked (if supported) exist only on relays with that capability;
` + "`" + s + "help`" + ` shows what is live here.

`)
	for _, c := range brokerCommands {
		b.WriteString("- " + c.Usage() + " — " + c.Help)
		if c.Need.optional() {
			b.WriteString(" (if supported)")
		}
		b.WriteString("\n")
	}
	b.WriteString(`
` + "`" + s + "login`" + ` is the relay's provider-OAuth login (model providers such as
anthropic). It is NOT how MCP servers authenticate — for those use the
` + "`" + s + "mcp`" + ` passthrough below.

## Agent passthrough commands

These are forwarded to the agent harness: ` + "`" + s + "<cmd> args`" + ` arrives as
` + "`/<cmd> args`" + `, arguments untouched. Only this allowlist is forwarded, and
only if the harness advertises the command:

`)
	for _, name := range PassthroughCommands() {
		b.WriteString("- `" + s + name + "` → `/" + name + "`\n")
	}
	b.WriteString(`
So a harness instruction like ` + "`/mcp login slack`" + ` becomes ` + "`" + s + "mcp login slack`" + `
for the user. Any other ` + "`/command`" + ` cannot be sent from chat; do not suggest it.
A passthrough works only if the harness advertises the command — the
user's ` + "`" + s + "help`" + ` lists the live ones under "Agent commands". If the one
you need is missing, tell the user it must be done in a terminal session
of the harness instead.
`)
	return b.String()
}

// SkillBundle returns an fs.FS laid out for skills.LoadBuiltinIn
// (bundle/<name>/SKILL.md) holding the generated relay-commands skill.
// Load it under its own appPrefix, distinct from the relay's own bundle.
func SkillBundle() fs.FS {
	return fstest.MapFS{"bundle/" + SkillName + "/SKILL.md": {Data: []byte(SkillMarkdown())}}
}
