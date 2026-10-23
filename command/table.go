package command

// Need names the relay capability a broker command depends on.
type Need int

const (
	// Always: the command is available on every broker.
	Always Need = iota
	// NeedController: requires a wired Controller.
	NeedController
	// NeedStopper: requires a Controller implementing TurnStopper.
	NeedStopper
	// NeedScheduler: requires a Controller and a Scheduler.
	NeedScheduler
)

// optional reports whether the command exists only on some relays.
func (n Need) optional() bool { return n == NeedStopper || n == NeedScheduler }

// BrokerCommand documents one command the broker itself answers (as
// opposed to a passthrough command forwarded to the agent).
type BrokerCommand struct {
	Verb  string // command word, without sigil
	Args  string // argument synopsis, "" if none
	Alias string // short alias, "" if none
	Help  string
	Need  Need
}

// Usage renders the command as the user types it, e.g. "`!model [filter|id]` (or `!m`)".
func (c BrokerCommand) Usage() string {
	u := "`" + DisplaySigil + c.Verb
	if c.Args != "" {
		u += " " + c.Args
	}
	u += "`"
	if c.Alias != "" {
		u += " (or `" + DisplaySigil + c.Alias + "`)"
	}
	return u
}

func (c BrokerCommand) avail(b *Broker) bool {
	switch c.Need {
	case NeedController:
		return b.ctrl != nil
	case NeedStopper:
		_, ok := b.stopper()
		return ok
	case NeedScheduler:
		_, ok := b.scheduler()
		return ok && b.ctrl != nil
	}
	return true
}

// brokerCommands is the single source of truth for !help and the
// relay-commands skill. Order is display order.
var brokerCommands = []BrokerCommand{
	{"help", "", "", "show this message", Always},
	{"status", "", "", "model, session and relay info", NeedController},
	{"model", "[filter|id]", "m", "list/filter models, or switch; fuzzy ids like `anth/opus55` work", NeedController},
	{"new", "", "", "start a fresh session (clears context)", NeedController},
	{"stop", "", "", "interrupt the turn currently running", NeedStopper},
	{"schedules", "", "", "list the prompts the agent has armed here", NeedScheduler},
	{"unschedule", "<id>", "", "cancel one of them", NeedScheduler},
	{"login", "[provider|cancel]", "", "connect a provider (e.g. `" + DisplaySigil + "login anthropic`), or abort a login in progress", Always},
}

// BrokerCommands returns a copy of the broker's own command table.
func BrokerCommands() []BrokerCommand {
	return append([]BrokerCommand(nil), brokerCommands...)
}
