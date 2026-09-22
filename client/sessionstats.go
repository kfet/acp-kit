package client

import (
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"
)

// AgentInfo is the agent's self-description from the initialize
// response (agentInfo). Every field is empty when the agent did not
// send one; the ACP spec makes it optional.
type AgentInfo struct {
	Name    string
	Title   string
	Version string
}

// parseAgentInfo extracts agentInfo from a raw initialize response.
func parseAgentInfo(raw json.RawMessage) AgentInfo {
	var env struct {
		AgentInfo *acp.Implementation `json:"agentInfo"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.AgentInfo == nil {
		return AgentInfo{}
	}
	ai := AgentInfo{Name: env.AgentInfo.Name, Version: env.AgentInfo.Version}
	if env.AgentInfo.Title != nil {
		ai.Title = *env.AgentInfo.Title
	}
	return ai
}

// AgentInfo returns what the agent reported about itself at Initialize.
func (a *AgentProc) AgentInfo() AgentInfo { return a.agentInfo }

// Cost is a cumulative session cost, as the agent reported it.
type Cost struct {
	Amount   float64
	Currency string // ISO 4217, e.g. "USD"
}

// SessionStats is what the agent has told the client about one
// session over ACP: its thinking level (a thought_level config
// option) and its latest usage_update. A zero field means the agent
// did not report it. The relay shows these facts; it never invents
// them.
type SessionStats struct {
	// Thinking is the current value of the thought_level config
	// option, e.g. "medium".
	Thinking string
	// ContextUsed and ContextSize are tokens in the context window
	// and the window size, from the latest usage_update.
	ContextUsed int
	ContextSize int
	// Cost is the cumulative session cost; nil when not reported.
	Cost *Cost
}

// thinkingFrom returns the current value of the thought_level select
// option, and false when there is none.
func thinkingFrom(opts []acp.SessionConfigOption) (string, bool) {
	for _, o := range opts {
		s := o.Select
		if s != nil && s.Category != nil && *s.Category == acp.SessionConfigOptionCategoryThoughtLevel {
			return string(s.CurrentValue), true
		}
	}
	return "", false
}

// noteConfig records the thinking level from a config option set.
// Caller holds a.mu.
func (a *AgentProc) noteConfig(sid acp.SessionId, opts []acp.SessionConfigOption) {
	if v, ok := thinkingFrom(opts); ok {
		st := a.stats[sid]
		st.Thinking = v
		a.stats[sid] = st
	}
}

// noteUpdate records the stats a session notification carries. It
// ignores a session with no sink (dropped or never registered), so a
// late update cannot re-add an entry that DropSession removed.
func (a *AgentProc) noteUpdate(sid acp.SessionId, u acp.SessionUpdate) {
	if u.ConfigOptionUpdate == nil && u.UsageUpdate == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sinks[sid]; !ok {
		return
	}
	if c := u.ConfigOptionUpdate; c != nil {
		a.noteConfig(sid, c.ConfigOptions)
	}
	if uu := u.UsageUpdate; uu != nil {
		st := a.stats[sid]
		st.ContextUsed, st.ContextSize = uu.Used, uu.Size
		if uu.Cost != nil {
			st.Cost = &Cost{Amount: uu.Cost.Amount, Currency: uu.Cost.Currency}
		}
		a.stats[sid] = st
	}
}

// SessionStats returns what the agent has reported about sid. The
// second result is false when it has reported nothing.
func (a *AgentProc) SessionStats(sid acp.SessionId) (SessionStats, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.stats[sid]
	return st, ok
}
