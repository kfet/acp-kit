package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/schedule"
)

// loopbackCtrl is a Controller that also implements the two OPTIONAL
// loopback capabilities, Poster and Scheduler.
type loopbackCtrl struct {
	fakeCtrl

	posted   [][2]string // {convID, text}
	postErr  error
	armed    []schedule.Item
	addErr   error
	unsched  [2]string
	unschErr error
	off      bool // scheduling implemented but switched off
}

func (c *loopbackCtrl) CanSchedule() bool { return !c.off }

func (c *loopbackCtrl) PostTo(conv, text string) error {
	c.posted = append(c.posted, [2]string{conv, text})
	return c.postErr
}

func (c *loopbackCtrl) Schedule(conv, text string, at time.Time, every time.Duration) (schedule.Item, error) {
	if c.addErr != nil {
		return schedule.Item{}, c.addErr
	}
	it := schedule.Item{ID: "s01", Conv: conv, Text: text, At: at, Every: every, Depth: 1}
	c.armed = append(c.armed, it)
	return it, nil
}

func (c *loopbackCtrl) Schedules(conv string) []schedule.Item {
	var out []schedule.Item
	for _, it := range c.armed {
		if it.Conv == conv {
			out = append(out, it)
		}
	}
	return out
}

func (c *loopbackCtrl) Unschedule(conv, id string) error {
	c.unsched = [2]string{conv, id}
	return c.unschErr
}

var when = time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

func TestActionsWithoutControllerAllFail(t *testing.T) {
	b := New(newFake())
	if _, err := b.Status("c"); !errors.Is(err, ErrNoController) {
		t.Fatalf("Status err = %v", err)
	}
	if _, err := b.ModelList(""); !errors.Is(err, ErrNoController) {
		t.Fatalf("ModelList err = %v", err)
	}
	if err := b.SelectModel("c", "m"); !errors.Is(err, ErrNoController) {
		t.Fatalf("SelectModel err = %v", err)
	}
	if err := b.NewSession("c"); !errors.Is(err, ErrNoController) {
		t.Fatalf("NewSession err = %v", err)
	}
	if b.CanPost() || b.CanSchedule() {
		t.Fatal("a broker with no controller must advertise no capabilities")
	}
	if err := b.Post("c", "hi"); err == nil {
		t.Fatal("want post refusal")
	}
	if _, err := b.Schedule("c", "x", when, 0); err == nil {
		t.Fatal("want schedule refusal")
	}
	if _, err := b.ScheduleList("c"); err == nil {
		t.Fatal("want list refusal")
	}
	if err := b.Unschedule("c", "s1"); err == nil {
		t.Fatal("want unschedule refusal")
	}
}

func TestCapabilitiesAreOptional(t *testing.T) {
	plain := withCtrl(&fakeCtrl{})
	if plain.CanPost() || plain.CanSchedule() {
		t.Fatal("a plain Controller must not advertise loopback capabilities")
	}
	full := withCtrl(&loopbackCtrl{})
	if !full.CanPost() || !full.CanSchedule() {
		t.Fatal("a loopback Controller must advertise both")
	}
}

func TestPost(t *testing.T) {
	c := &loopbackCtrl{}
	b := withCtrl(c)
	if err := b.Post("conv", "  "); err == nil {
		t.Fatal("want refusal for empty text")
	}
	if err := b.Post("conv", "hello"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(c.posted) != 1 || c.posted[0] != [2]string{"conv", "hello"} {
		t.Fatalf("posted = %v", c.posted)
	}
	c.postErr = errors.New("zulip said no")
	if err := b.Post("conv", "hello"); err == nil {
		t.Fatal("want the relay's error surfaced")
	}
}

func TestScheduleActions(t *testing.T) {
	c := &loopbackCtrl{}
	b := withCtrl(c)
	it, err := b.Schedule("conv", "check the deploy", when, time.Hour)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if it.ID != "s01" {
		t.Fatalf("item = %+v", it)
	}
	list, err := b.ScheduleList("conv")
	if err != nil || len(list) != 1 {
		t.Fatalf("ScheduleList = %v, %v", list, err)
	}
	if got, _ := b.ScheduleList("elsewhere"); len(got) != 0 {
		t.Fatal("listing leaked another conversation's schedules")
	}
	if err := b.Unschedule("conv", ""); err == nil {
		t.Fatal("want refusal for a missing id")
	}
	if err := b.Unschedule("conv", "s01"); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
	if c.unsched != [2]string{"conv", "s01"} {
		t.Fatalf("unsched = %v", c.unsched)
	}
	c.addErr = errors.New("cap reached")
	if _, err := b.Schedule("conv", "x", when, 0); err == nil {
		t.Fatal("want the store's error surfaced")
	}
}

// TestStatusMentionsSchedules proves the two front ends share one
// implementation: the schedule count appears in the same rendering the
// `!status` command prints and the status tool returns.
func TestStatusMentionsSchedules(t *testing.T) {
	c := &loopbackCtrl{}
	b := withCtrl(c)
	got, err := b.Status("conv")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if strings.Contains(got, "schedules armed") {
		t.Fatalf("empty schedule set should not be mentioned: %s", got)
	}
	if _, err := b.Schedule("conv", "later", when, 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	got, err = b.Status("conv")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(got, "schedules armed here: 1") {
		t.Fatalf("status did not mention the armed schedule: %s", got)
	}
	// A Controller with no scheduler must not blow up on the same path.
	if _, err := withCtrl(&fakeCtrl{}).Status("conv"); err != nil {
		t.Fatalf("plain Status: %v", err)
	}
}

func TestRenderSchedules(t *testing.T) {
	if got := RenderSchedules(nil); got != "Nothing scheduled here." {
		t.Fatalf("empty listing = %q", got)
	}
	long := strings.Repeat("word ", 60)
	got := RenderSchedules([]schedule.Item{
		{ID: "s01", At: when, Text: "one\n\ttwo"},
		{ID: "s02", At: when, Every: time.Hour, Fires: 3, Text: long},
	})
	for _, want := range []string{"`s01`", "2026-09-02 09:00 UTC", "one two", "every 1h0m0s", "fired 3×", "…"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing missing %q:\n%s", want, got)
		}
	}
}

// --- chat front end ------------------------------------------------------

func TestScheduleCommandsNeedTheCapability(t *testing.T) {
	plain := withCtrl(&fakeCtrl{})
	for _, text := range []string{"!schedules", "!schedule", "!unschedule s01"} {
		if plain.IsCommand(text) {
			t.Fatalf("%q must not be a command without a Scheduler", text)
		}
	}
	full := withCtrl(&loopbackCtrl{})
	for _, text := range []string{"!schedules", "!schedule", "!unschedule s01"} {
		if !full.IsCommand(text) {
			t.Fatalf("%q should be a command with a Scheduler", text)
		}
	}
}

func TestScheduleCommands(t *testing.T) {
	c := &loopbackCtrl{}
	b := withCtrl(c)
	ctx := context.Background()

	out, err := b.Handle(ctx, "conv", "!schedules")
	if err != nil || out == nil || !strings.Contains(out.Text, "Nothing scheduled") {
		t.Fatalf("empty !schedules = %+v, %v", out, err)
	}
	if _, err := b.Schedule("conv", "check the deploy", when, 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	out, _ = b.Handle(ctx, "conv", "!SCHEDULE")
	if !strings.Contains(out.Text, "`s01`") || !strings.Contains(out.Text, "check the deploy") {
		t.Fatalf("!schedule listing = %s", out.Text)
	}

	out, _ = b.Handle(ctx, "conv", "!unschedule s01")
	if !strings.Contains(out.Text, "Cancelled `s01`") {
		t.Fatalf("!unschedule = %s", out.Text)
	}
	c.unschErr = errors.New("no schedule \"s01\" here")
	out, _ = b.Handle(ctx, "conv", "!unschedule s01")
	if !strings.Contains(out.Text, "❌") {
		t.Fatalf("failed !unschedule = %s", out.Text)
	}
}

func TestSchedulesCommandWithoutCapabilityReportsIt(t *testing.T) {
	// Reached only by calling the renderer directly: IsCommand refuses
	// to classify it, so a user cannot get here. Covered so the message
	// stays honest if that ever changes.
	b := withCtrl(&fakeCtrl{})
	if got := b.schedules("conv"); !strings.Contains(got.Text, "cannot schedule") {
		t.Fatalf("schedules() = %s", got.Text)
	}
}

func TestHelpListsScheduleCommands(t *testing.T) {
	with := withCtrl(&loopbackCtrl{}).help().Text
	for _, want := range []string{"!schedules", "!unschedule"} {
		if !strings.Contains(with, want) {
			t.Fatalf("help missing %q: %s", want, with)
		}
	}
	without := withCtrl(&fakeCtrl{}).help().Text
	if strings.Contains(without, "!schedules") {
		t.Fatalf("help advertised scheduling with no Scheduler: %s", without)
	}
}

func TestCapitalise(t *testing.T) {
	if got := capitalise("session control is unavailable"); got != "Session control is unavailable" {
		t.Fatalf("capitalise = %q", got)
	}
	if got := capitalise(""); got != "" {
		t.Fatalf("capitalise(empty) = %q", got)
	}
	if got := capitalise("❌ nope"); got != "❌ nope" {
		t.Fatalf("capitalise(non-ascii) = %q", got)
	}
}

// TestSchedulingCanBeSwitchedOff: a relay that implements Scheduler but
// has the feature off must advertise nothing. A type assertion can only
// say "could"; CanSchedule says "and does".
func TestSchedulingCanBeSwitchedOff(t *testing.T) {
	b := withCtrl(&loopbackCtrl{off: true})
	if b.CanSchedule() {
		t.Fatal("a switched-off Scheduler must not advertise the capability")
	}
	for _, text := range []string{"!schedules", "!unschedule s01"} {
		if b.IsCommand(text) {
			t.Fatalf("%q must not be a command when scheduling is off", text)
		}
	}
	if strings.Contains(b.help().Text, "!schedules") {
		t.Fatalf("help advertised scheduling while off: %s", b.help().Text)
	}
	if _, err := b.ScheduleList("conv"); err == nil {
		t.Fatal("want a refusal while off")
	}
	if _, err := b.Schedule("conv", "x", when, 0); err == nil {
		t.Fatal("want a refusal while off")
	}
	if err := b.Unschedule("conv", "s01"); err == nil {
		t.Fatal("want a refusal while off")
	}
	// Status must not mention schedules it cannot see.
	got, err := b.Status("conv")
	if err != nil || strings.Contains(got, "schedules armed") {
		t.Fatalf("status = %q, %v", got, err)
	}
}
