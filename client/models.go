package client

import (
	acp "github.com/coder/acp-go-sdk"
)

// Model handling spans two generations of the ACP wire protocol, and
// acp-kit speaks both:
//
//   - old (acp-go-sdk <= v0.12, still what `fir` emits): session/new and
//     session/resume carry a "models" object ({availableModels,
//     currentModelId}) and the model is changed with the session/set_model
//     RPC. The SDK dropped these types in v0.13, so the small structs are
//     declared locally here.
//   - new (acp-go-sdk >= v0.13): session/new and session/resume carry
//     "configOptions"; the model is the Select option whose category is
//     "model", and it is changed with the session/set_config_option RPC.
//
// New-style wins when both are present; otherwise we fall back to old-style.

// modelState is acp-kit's protocol-agnostic snapshot of an agent's model
// list. configID is set only for new-style agents and names the session
// config option that selects the model.
type modelState struct {
	models   []ModelInfo
	current  string
	configID string
}

// legacyModelInfo is one entry of the old-style "models.availableModels".
type legacyModelInfo struct {
	ModelId string `json:"modelId"`
	Name    string `json:"name"`
}

// legacyModelState mirrors the old-style "models" object that acp-go-sdk
// modelled as SessionModelState before v0.13 removed it.
type legacyModelState struct {
	AvailableModels []legacyModelInfo `json:"availableModels"`
	CurrentModelId  string            `json:"currentModelId"`
}

// sessionResponse is the subset of a session/new or session/resume response
// acp-kit reads. It is decoded from the raw JSON so both protocol
// generations can be inspected regardless of the SDK's typed structs.
type sessionResponse struct {
	SessionId     acp.SessionId             `json:"sessionId"`
	ConfigOptions []acp.SessionConfigOption `json:"configOptions"`
	Models        *legacyModelState         `json:"models"`
}

// modelState extracts a model snapshot from the response, preferring the
// new-style config options. Returns nil when the agent reported neither.
func (r sessionResponse) modelState() *modelState {
	if ms := modelsFromConfigOptions(r.ConfigOptions); ms != nil {
		return ms
	}
	return modelsFromLegacy(r.Models)
}

// modelsFromConfigOptions picks the Select config option categorised as
// "model" and maps it to a modelState. Returns nil when there is none.
func modelsFromConfigOptions(opts []acp.SessionConfigOption) *modelState {
	for _, o := range opts {
		s := o.Select
		if s == nil || s.Category == nil || *s.Category != acp.SessionConfigOptionCategoryModel {
			continue
		}
		ms := &modelState{current: string(s.CurrentValue), configID: string(s.Id)}
		for _, v := range selectOptionValues(s.Options) {
			ms.models = append(ms.models, ModelInfo{ID: string(v.Value), Name: v.Name})
		}
		return ms
	}
	return nil
}

// selectOptionValues flattens the grouped/ungrouped select-options union
// into a flat value list.
func selectOptionValues(o acp.SessionConfigSelectOptions) []acp.SessionConfigSelectOption {
	if o.Ungrouped != nil {
		return *o.Ungrouped
	}
	if o.Grouped == nil {
		return nil
	}
	var out []acp.SessionConfigSelectOption
	for _, g := range *o.Grouped {
		out = append(out, g.Options...)
	}
	return out
}

// modelsFromLegacy maps an old-style "models" object to a modelState.
func modelsFromLegacy(m *legacyModelState) *modelState {
	if m == nil {
		return nil
	}
	ms := &modelState{current: m.CurrentModelId}
	for _, x := range m.AvailableModels {
		ms.models = append(ms.models, ModelInfo{ID: x.ModelId, Name: x.Name})
	}
	return ms
}

// setSessionModelRequest is the params for the old-style session/set_model
// RPC (acp.UnstableSetSessionModelRequest before the SDK dropped it).
type setSessionModelRequest struct {
	SessionId acp.SessionId `json:"sessionId"`
	ModelId   string        `json:"modelId"`
}

// agentMethodSessionSetModel is the old-style set-model RPC, removed from
// the SDK's constants in v0.13 but still spoken by older agents.
const agentMethodSessionSetModel = "session/set_model"
