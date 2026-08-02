package client

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func ptrCategory(c acp.SessionConfigOptionCategory) *acp.SessionConfigOptionCategory { return &c }

// newStyleAgent answers session/new and session/resume with ACP >= 0.13
// "configOptions" instead of the old "models" object, and records the
// params of the set-config-option RPC.
func newStyleAgent(rec *sync.Map) func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
	configOptions := []map[string]any{
		{
			"type":         "boolean",
			"id":           "web_search",
			"name":         "Web search",
			"currentValue": true,
		},
		{
			"type":         "select",
			"id":           "model",
			"name":         "Model",
			"category":     "model",
			"currentValue": "p/m2",
			"options": []map[string]any{
				{"value": "p/m1", "name": "Model 1"},
				{"value": "p/m2", "name": "Model 2"},
			},
		},
	}
	return func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{
					"sessionCapabilities": map[string]any{"resume": map[string]any{}},
				},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{"sessionId": "sess-N", "configOptions": configOptions}, nil
		case "session/resume":
			return map[string]any{"configOptions": configOptions}, nil
		case acp.AgentMethodSessionSetConfigOption:
			rec.Store("params", string(params))
			return map[string]any{"configOptions": configOptions}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
}

func TestNewStyleConfigOptionModels(t *testing.T) {
	var rec sync.Map
	pc := startPaired(t, Config{Command: []string{"x"}}, newStyleAgent(&rec))
	a := pc.agent
	ctx := context.Background()

	sid, err := a.NewSession(ctx, "/cwd", &recSink{}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	models, cur := a.Models()
	if len(models) != 2 || models[0].ID != "p/m1" || models[0].Name != "Model 1" || cur != "p/m2" {
		t.Fatalf("models = %#v cur = %q", models, cur)
	}

	// SetModel must route through session/set_config_option with the id
	// of the "model" config option.
	if err := a.SetModel(ctx, sid, "p/m1"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	got, _ := rec.Load("params")
	var sent setSessionConfigOptionRequest
	if err := json.Unmarshal([]byte(got.(string)), &sent); err != nil {
		t.Fatalf("unmarshal recorded params: %v", err)
	}
	if sent.ConfigId != "model" || sent.Value != "p/m1" || sent.SessionId != sid {
		t.Fatalf("set_config_option params = %#v", sent)
	}
}

func TestNewStyleConfigOptionModelsOnResume(t *testing.T) {
	var rec sync.Map
	pc := startPaired(t, Config{Command: []string{"x"}}, newStyleAgent(&rec))
	a := pc.agent

	if err := a.ResumeSession(context.Background(), "/cwd", "s1", &recSink{}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if models, cur := a.Models(); len(models) != 2 || cur != "p/m2" {
		t.Fatalf("models = %#v cur = %q", models, cur)
	}
}

func TestModelStatePrefersConfigOptions(t *testing.T) {
	raw := []byte(`{
		"sessionId": "s",
		"models": {"availableModels":[{"modelId":"old/a","name":"Old"}],"currentModelId":"old/a"},
		"configOptions": [
			{"type":"select","id":"model","name":"Model","category":"model",
			 "currentValue":"new/b","options":[{"value":"new/b","name":"New"}]}
		]
	}`)
	var r sessionResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ms := r.modelState()
	if ms == nil || ms.current != "new/b" || ms.configID != "model" || len(ms.models) != 1 {
		t.Fatalf("modelState = %#v", ms)
	}
}

func TestModelStateNoneReported(t *testing.T) {
	var r sessionResponse
	if err := json.Unmarshal([]byte(`{"sessionId":"s"}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ms := r.modelState(); ms != nil {
		t.Fatalf("expected nil modelState, got %#v", ms)
	}
}

func TestModelsFromConfigOptionsSkipsNonModel(t *testing.T) {
	opts := []acp.SessionConfigOption{
		{Boolean: &acp.SessionConfigOptionBoolean{Id: "b"}},
		{Select: &acp.SessionConfigOptionSelect{Id: "no-category"}},
		{Select: &acp.SessionConfigOptionSelect{
			Id:       "mode",
			Category: ptrCategory(acp.SessionConfigOptionCategoryMode),
		}},
	}
	if ms := modelsFromConfigOptions(opts); ms != nil {
		t.Fatalf("expected nil, got %#v", ms)
	}
}

func TestSelectOptionValuesGrouped(t *testing.T) {
	grouped := acp.SessionConfigSelectOptionsGrouped{
		{Group: "g1", Name: "Group 1", Options: []acp.SessionConfigSelectOption{{Value: "a", Name: "A"}}},
		{Group: "g2", Name: "Group 2", Options: []acp.SessionConfigSelectOption{{Value: "b", Name: "B"}}},
	}
	ms := modelsFromConfigOptions([]acp.SessionConfigOption{{
		Select: &acp.SessionConfigOptionSelect{
			Id:           "model",
			Category:     ptrCategory(acp.SessionConfigOptionCategoryModel),
			CurrentValue: "b",
			Options:      acp.SessionConfigSelectOptions{Grouped: &grouped},
		},
	}})
	if ms == nil || len(ms.models) != 2 || ms.models[0].ID != "a" || ms.models[1].ID != "b" {
		t.Fatalf("modelState = %#v", ms)
	}
}

func TestSelectOptionValuesEmptyUnion(t *testing.T) {
	if vs := selectOptionValues(acp.SessionConfigSelectOptions{}); vs != nil {
		t.Fatalf("expected nil, got %#v", vs)
	}
}

func TestModelsFromLegacyNil(t *testing.T) {
	if ms := modelsFromLegacy(nil); ms != nil {
		t.Fatalf("expected nil, got %#v", ms)
	}
}
