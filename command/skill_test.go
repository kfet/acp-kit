package command

import (
	"strings"
	"testing"

	"github.com/kfet/acp-kit/skills"
)

func TestSkillListsEveryAllowlistedCommand(t *testing.T) {
	md := SkillMarkdown()
	if len(passthroughAllow) == 0 {
		t.Fatal("empty allowlist")
	}
	for name := range passthroughAllow {
		if !strings.Contains(md, "`!"+name+"` → `/"+name+"`") {
			t.Errorf("skill missing passthrough %q", name)
		}
	}
	for _, c := range BrokerCommands() {
		if !strings.Contains(md, c.Usage()) {
			t.Errorf("skill missing broker command %q", c.Verb)
		}
	}
	for _, want := range []string{"!mcp login slack", "NOT how MCP servers authenticate", "(if supported)"} {
		if !strings.Contains(md, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillBundleLoads(t *testing.T) {
	got, err := skills.LoadBuiltinIn(t.TempDir(), SkillBundle(), "test-relay")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != SkillName || !strings.Contains(got[0].Description, "MCP server") {
		t.Fatalf("got %+v", got)
	}
}

// Every verb the broker recognises must be in the table, so neither
// !help nor the skill can miss a newly added command.
func TestTableCoversRecognisedVerbs(t *testing.T) {
	b := withCtrl(&loopbackCtrl{})
	verbs := map[string]bool{}
	for _, c := range BrokerCommands() {
		verbs[c.Verb] = true
	}
	for _, v := range []string{"help", "status", "model", "new", "schedules", "unschedule", "login"} {
		if !b.IsCommand("!"+v) || !verbs[v] {
			t.Errorf("verb %q: recognised=%v inTable=%v", v, b.IsCommand("!"+v), verbs[v])
		}
	}
	if !verbs["stop"] || !withCtrl(&stoppingCtrl{}).IsCommand("!stop") {
		t.Error("stop")
	}
}

func TestHelpWithoutController(t *testing.T) {
	h := New(nil).help().Text
	for _, bad := range []string{"!status", "!new", "!stop", "!schedules"} {
		if strings.Contains(h, bad) {
			t.Errorf("help without controller lists %s", bad)
		}
	}
	if !strings.Contains(h, "!help") || !strings.Contains(h, "!login") {
		t.Errorf("help: %s", h)
	}
}
