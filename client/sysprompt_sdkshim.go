package client

// SDK SHIM for the stable ACP client-provided system prompt (ACP v1,
// stabilized 2026-11-06).
//
// coder/acp-go-sdk v0.13.5 has no type for the capability
// agentCapabilities.sessionCapabilities.systemPrompt or for the
// params.systemPrompt request field. This file reads the capability from
// the raw initialize response and wraps the SDK request structs to add
// the field. Remove this file (and the SystemPrompt fields on the local
// resumeSessionRequest / forkSessionRequest) once the SDK models them;
// switch the callers to the SDK types.

import (
	"bytes"
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"
)

// systemPromptParam is the wire shape of params.systemPrompt.
type systemPromptParam struct {
	Mode    string             `json:"mode"`
	Content []acp.ContentBlock `json:"content"`
}

// newSessionRequest is acp.NewSessionRequest plus the systemPrompt field.
type newSessionRequest struct {
	acp.NewSessionRequest
	SystemPrompt *systemPromptParam `json:"systemPrompt,omitempty"`
}

// loadSessionRequest is acp.LoadSessionRequest plus the systemPrompt field.
type loadSessionRequest struct {
	acp.LoadSessionRequest
	SystemPrompt *systemPromptParam `json:"systemPrompt,omitempty"`
}

// parseSystemPromptCap reads agentCapabilities.sessionCapabilities.systemPrompt
// from a raw initialize response. present is true for a JSON object (absent,
// null or any non-object value means unsupported); override is true when
// its mode.override is an object too.
func parseSystemPromptCap(raw json.RawMessage) (present, override bool) {
	var env struct {
		AgentCapabilities struct {
			SessionCapabilities struct {
				SystemPrompt json.RawMessage `json:"systemPrompt"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &env)
	sp := env.AgentCapabilities.SessionCapabilities.SystemPrompt
	if !isJSONObject(sp) {
		return false, false
	}
	var c struct {
		Mode struct {
			Override json.RawMessage `json:"override"`
		} `json:"mode"`
	}
	_ = json.Unmarshal(sp, &c)
	return true, isJSONObject(c.Mode.Override)
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}
