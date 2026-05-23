package client

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func mkReq(title string, opts ...acp.PermissionOption) acp.RequestPermissionRequest {
	req := acp.RequestPermissionRequest{Options: opts}
	if title != "" {
		req.ToolCall.Title = &title
	}
	return req
}

func selectedID(resp acp.RequestPermissionResponse) acp.PermissionOptionId {
	if resp.Outcome.Selected == nil {
		return ""
	}
	return resp.Outcome.Selected.OptionId
}

func TestAllowAllPermissions(t *testing.T) {
	allow := acp.PermissionOption{OptionId: "a", Name: "Allow once", Kind: "allow_once"}
	reject := acp.PermissionOption{OptionId: "r", Name: "Reject once", Kind: "reject_once"}
	got := AllowAllPermissions(context.Background(), mkReq("", reject, allow))
	if selectedID(got) != "a" {
		t.Fatalf("got %q want a", selectedID(got))
	}
}

func TestDenyAllPermissions(t *testing.T) {
	allow := acp.PermissionOption{OptionId: "a", Name: "Allow once", Kind: "allow_once"}
	reject := acp.PermissionOption{OptionId: "r", Name: "Reject once", Kind: "reject_once"}
	got := DenyAllPermissions(context.Background(), mkReq("", allow, reject))
	if selectedID(got) != "r" {
		t.Fatalf("got %q want r", selectedID(got))
	}
}

func TestReadOnlyPermissions(t *testing.T) {
	allow := acp.PermissionOption{OptionId: "a", Name: "allow", Kind: "allow_once"}
	reject := acp.PermissionOption{OptionId: "r", Name: "reject", Kind: "reject_once"}

	cases := []struct {
		title string
		want  acp.PermissionOptionId
	}{
		{"Read file foo", "a"},
		{"List directory", "a"},
		{"Write file foo", "r"},
		{"Edit file", "r"},
		{"Bash command", "r"},
		{"Exec sh", "r"},
		{"Run script", "r"},
		{"Delete file", "r"},
		{"rm file", "r"},
		{"", "a"}, // empty title → allowed
	}
	for _, c := range cases {
		got := ReadOnlyPermissions(context.Background(), mkReq(c.title, allow, reject))
		if selectedID(got) != c.want {
			t.Errorf("title %q: got %q want %q", c.title, selectedID(got), c.want)
		}
	}
}

func TestPickPermission_FallsBackToFirstWhenNoMatch(t *testing.T) {
	// Options carry no "allow" or "approve" markers — picker falls back to first.
	a := acp.PermissionOption{OptionId: "first", Name: "x", Kind: "y"}
	b := acp.PermissionOption{OptionId: "second", Name: "z", Kind: "w"}
	got := pickPermission(mkReq("", a, b), "allow")
	if selectedID(got) != "first" {
		t.Fatalf("got %q want first", selectedID(got))
	}
}

func TestPickPermission_EmptyOptionsReturnsEmpty(t *testing.T) {
	got := pickPermission(mkReq(""), "allow")
	if selectedID(got) != "" {
		t.Fatalf("got %q want empty", selectedID(got))
	}
}
