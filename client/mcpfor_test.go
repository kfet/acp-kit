package client

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestConfig_mcpFor(t *testing.T) {
	// nil hook → empty, non-nil
	got := Config{}.mcpFor("/c")
	if got == nil || len(got) != 0 {
		t.Fatalf("nil hook: want empty non-nil, got %v", got)
	}

	// hook returns nil → empty, non-nil
	got = Config{MCPServersForSession: func(string) []acp.McpServer { return nil }}.mcpFor("/c")
	if got == nil || len(got) != 0 {
		t.Fatalf("nil-returning hook: want empty non-nil, got %v", got)
	}

	// hook returns servers → passed through, cwd forwarded
	var gotCwd string
	got = Config{MCPServersForSession: func(cwd string) []acp.McpServer {
		gotCwd = cwd
		return []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "x", Command: "c"}}}
	}}.mcpFor("/conv/abc")
	if len(got) != 1 || got[0].Stdio == nil || got[0].Stdio.Name != "x" {
		t.Fatalf("hook servers not forwarded: %v", got)
	}
	if gotCwd != "/conv/abc" {
		t.Fatalf("cwd = %q", gotCwd)
	}
}
