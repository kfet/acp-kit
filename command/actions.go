// This file is the session-control surface as ACTIONS rather than as
// chat text.
//
// # Why it exists
//
// The relay now has two front ends onto the same controls: the
// `!command` a human types, and the self-hosted MCP tools an agent
// calls mid-turn (see acp-kit/relaytool). If those two grew their own
// implementations they would drift — which is exactly the mistake the
// pre-v0.6.0 relays made with the command surface itself, and exactly
// what this package was created to correct. So there is one
// implementation: the exported methods below. The `!` handlers in
// command.go render their results as chat prose; relaytool hands the
// same results to the agent. Neither reaches the Controller directly.
//
// The rule to keep: nothing in command.go may call b.ctrl for anything
// an action here already does.
package command

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/schedule"
)

// ErrNoController is returned by every action when the relay wired no
// Controller. The chat front end renders it as prose; the MCP front end
// surfaces it as a tool error.
var ErrNoController = errors.New("session control is unavailable")

// Poster is the OPTIONAL capability behind the agent-facing `post`
// tool: it sends a message into a conversation OUT OF BAND, outside
// any turn's streamed answer. It is what makes "go do X and tell me
// when it lands" expressible — progress on a long turn, or a result
// that arrives after the turn that started it has ended.
//
// It is optional because not every relay can do it. A relay that
// answers one HTTP request per turn (poe-acp) has no channel to speak
// on once that request is over, so it must not advertise the tool.
//
// # Blast radius
//
// PostTo takes the conversation token the tool call was bound to and
// NOTHING else. There is deliberately no target parameter: an agent
// that can post into arbitrary channels is a new and serious
// capability — a prompt-injected agent would gain a realm-wide
// megaphone — so in v1 it is not gated behind config, it is simply not
// expressible. Widening this later means adding a parameter, a config
// allowlist and a threat model, in that order.
type Poster interface {
	PostTo(convID, text string) error
}

// Scheduler is the OPTIONAL capability behind scheduled prompts. A
// relay implements it over acp-kit/schedule; see that package for the
// runaway bounds, which are the reason this is not simply a timer.
//
// Optional for the same reason as Poster: firing a schedule means
// injecting a prompt into a conversation unprompted, which a
// request/response relay cannot do.
type Scheduler interface {
	// CanSchedule reports whether scheduling is available RIGHT NOW.
	//
	// A type assertion can only answer "this relay could schedule";
	// it cannot answer "and the operator turned it on". zulip-acp
	// implements Scheduler unconditionally but only has a store when
	// `relay_mcp` is set, and without this the chat surface would
	// advertise `!schedules` on a relay where nothing can ever be
	// armed. Everything gates on it in one place — see Broker.scheduler
	// — so `!help`, the commands, Status and the MCP tools appear and
	// disappear together.
	CanSchedule() bool
	// Schedule arms a prompt. Depth and the caps are enforced by the
	// store, not by the caller.
	Schedule(convID, text string, at time.Time, every time.Duration) (schedule.Item, error)
	// Schedules lists what is armed for the conversation.
	Schedules(convID string) []schedule.Item
	// Unschedule disarms one item, scoped to the conversation.
	Unschedule(convID, id string) error
}

// poster returns the Controller's Poster capability, if it has one.
func (b *Broker) poster() (Poster, bool) {
	if b.ctrl == nil {
		return nil, false
	}
	p, ok := b.ctrl.(Poster)
	return p, ok
}

// scheduler returns the Controller's Scheduler capability, if it has
// one AND scheduling is actually switched on. This is the single gate:
// every other mention of scheduling in this package goes through it.
func (b *Broker) scheduler() (Scheduler, bool) {
	if b.ctrl == nil {
		return nil, false
	}
	s, ok := b.ctrl.(Scheduler)
	if !ok || !s.CanSchedule() {
		return nil, false
	}
	return s, true
}

// CanPost reports whether the relay can post out of band.
func (b *Broker) CanPost() bool { _, ok := b.poster(); return ok }

// CanSchedule reports whether the relay can schedule prompts.
func (b *Broker) CanSchedule() bool { _, ok := b.scheduler(); return ok }

// --- actions -------------------------------------------------------------

// Status renders one compact snapshot of everything a caller might ask
// about: the conversation's model / thinking / session plus the relay
// process's version / uptime / conversations.
//
// Rendered as a short bullet list — it has to read well on a phone, so
// no tables and no wide lines; conversation state first, relay after.
// The agent reads the same markdown, which is fine: it is the same
// facts, and a second "structured" rendering would be a second thing to
// keep in step.
func (b *Broker) Status(convID string) (string, error) {
	if b.ctrl == nil {
		return "", ErrNoController
	}
	st := b.ctrl.StatusFor(convID)
	ri := b.ctrl.RelayInfo(convID)
	var sb strings.Builder
	sb.WriteString("**Status**\n\n")
	if st.Where != "" {
		fmt.Fprintf(&sb, "- here: %s\n", st.Where)
	}
	fmt.Fprintf(&sb, "- model: `%s`", st.EffectiveModel)
	if st.OverrideModel != "" {
		fmt.Fprintf(&sb, " (set via %smodel)", DisplaySigil)
	}
	sb.WriteString("\n")
	if st.Thinking != "" {
		fmt.Fprintf(&sb, "- thinking: %s\n", st.Thinking)
	}
	sess := "none yet (fresh on next message)"
	if st.HasSession {
		sess = "active"
	}
	if ri.SessionID != "" {
		sess += " `" + ri.SessionID + "`"
	}
	fmt.Fprintf(&sb, "- session: %s\n", sess)
	if st.ConvID != "" {
		fmt.Fprintf(&sb, "- conversation: `%s`\n", st.ConvID)
	}
	if st.StateDir != "" {
		fmt.Fprintf(&sb, "- state dir: `%s`\n", st.StateDir)
	}
	if st.TurnRunning {
		fmt.Fprintf(&sb, "- turn running: yes — `%sstop` interrupts it\n", DisplaySigil)
	}
	fmt.Fprintf(&sb, "- models available: %d\n", st.ModelsAvailable)
	if ri.Version != "" {
		fmt.Fprintf(&sb, "- relay: `%s`\n", ri.Version)
	}
	if ri.Uptime != "" {
		fmt.Fprintf(&sb, "- uptime: %s\n", ri.Uptime)
	}
	if ri.AgentCmd != "" {
		fmt.Fprintf(&sb, "- agent: `%s`\n", ri.AgentCmd)
	}
	fmt.Fprintf(&sb, "- active conversations: %d\n", ri.ActiveSessions)
	if n := len(b.scheduleList(convID)); n > 0 {
		fmt.Fprintf(&sb, "- schedules armed here: %d — `%sschedules` lists them\n", n, DisplaySigil)
	}
	return sb.String(), nil
}

// ModelList renders the available model ids, optionally filtered by
// substring.
func (b *Broker) ModelList(filter string) (string, error) {
	if b.ctrl == nil {
		return "", ErrNoController
	}
	all, current := b.ctrl.AvailableModels()
	if len(all) == 0 {
		return fmt.Sprintf("No models available — connect a provider with `%slogin`.", DisplaySigil), nil
	}
	f := strings.ToLower(filter)
	matched := make([]client.ModelInfo, 0, len(all))
	for _, m := range all {
		if f == "" || strings.Contains(strings.ToLower(m.ID), f) {
			matched = append(matched, m)
		}
	}
	var sb strings.Builder
	if filter == "" {
		fmt.Fprintf(&sb, "%d models available (current: `%s`). `%smodel <id>` to switch, `%smodel <filter>` to narrow:\n\n",
			len(all), current, DisplaySigil, DisplaySigil)
	} else {
		fmt.Fprintf(&sb, "%d model(s) match %q (current: `%s`):\n\n", len(matched), filter, current)
	}
	if len(matched) == 0 {
		fmt.Fprintf(&sb, "(none match %q — try `%smodel` for the full list)\n", filter, DisplaySigil)
		return sb.String(), nil
	}
	for i, m := range matched {
		if i >= modelsListCap {
			fmt.Fprintf(&sb, "…and %d more (filter to narrow).\n", len(matched)-modelsListCap)
			break
		}
		marker := ""
		if m.ID == current {
			marker = " ←"
		}
		fmt.Fprintf(&sb, "- `%s`%s\n", m.ID, marker)
	}
	return sb.String(), nil
}

// SelectModel switches the sticky model for the conversation.
func (b *Broker) SelectModel(convID, modelID string) error {
	if b.ctrl == nil {
		return ErrNoController
	}
	return b.ctrl.SetModelOverride(convID, modelID)
}

// NewSession drops the conversation's live session so the next turn
// starts fresh.
func (b *Broker) NewSession(convID string) error {
	if b.ctrl == nil {
		return ErrNoController
	}
	return b.ctrl.ResetSession(convID)
}

// Post sends a message into the conversation out of band. See Poster
// for why there is no target.
func (b *Broker) Post(convID, text string) error {
	p, ok := b.poster()
	if !ok {
		return errors.New("this relay cannot post out of band")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to post")
	}
	return p.PostTo(convID, text)
}

// Schedule arms a prompt to be injected into the conversation later.
func (b *Broker) Schedule(convID, text string, at time.Time, every time.Duration) (schedule.Item, error) {
	s, ok := b.scheduler()
	if !ok {
		return schedule.Item{}, errors.New("this relay cannot schedule prompts")
	}
	return s.Schedule(convID, text, at, every)
}

// ScheduleList returns the conversation's armed schedules.
func (b *Broker) ScheduleList(convID string) ([]schedule.Item, error) {
	if _, ok := b.scheduler(); !ok {
		return nil, errors.New("this relay cannot schedule prompts")
	}
	return b.scheduleList(convID), nil
}

// scheduleList is the error-free read used by Status, which mentions
// schedules only when there are any and must not care whether the
// capability exists.
func (b *Broker) scheduleList(convID string) []schedule.Item {
	s, ok := b.scheduler()
	if !ok {
		return nil
	}
	return s.Schedules(convID)
}

// Unschedule disarms one of the conversation's schedules.
func (b *Broker) Unschedule(convID, id string) error {
	s, ok := b.scheduler()
	if !ok {
		return errors.New("this relay cannot schedule prompts")
	}
	if id == "" {
		return errors.New("which one? pass the id from the schedules listing")
	}
	return s.Unschedule(convID, id)
}

// RenderSchedules renders an armed-schedule listing for a human.
//
// Times are absolute UTC rather than relative ("in 12m") on purpose:
// the listing is also what the agent reads back through
// list_schedules, and a relative time is only true at the instant it
// is rendered.
func RenderSchedules(items []schedule.Item) string {
	if len(items) == 0 {
		return "Nothing scheduled here."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Scheduled here** (%d) — `%sunschedule <id>` cancels one:\n\n", len(items), DisplaySigil)
	for _, it := range items {
		fmt.Fprintf(&sb, "- `%s` — %s", it.ID, it.At.UTC().Format("2006-01-02 15:04 MST"))
		if it.Every > 0 {
			fmt.Fprintf(&sb, ", every %s", it.Every)
		}
		if it.Fires > 0 {
			fmt.Fprintf(&sb, ", fired %d×", it.Fires)
		}
		fmt.Fprintf(&sb, "\n  %s\n", oneLine(it.Text))
	}
	return sb.String()
}

// scheduleSummaryCap bounds how much of a scheduled prompt a listing
// echoes back.
const scheduleSummaryCap = 120

// oneLine flattens a prompt to a single truncated line, so a listing of
// ten schedules stays readable on a phone.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > scheduleSummaryCap {
		return s[:scheduleSummaryCap] + "…"
	}
	return s
}
