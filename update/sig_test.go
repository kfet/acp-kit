package update

import (
	"os/signal"
	"syscall"
)

func signalIgnoreHUP() { signal.Ignore(syscall.SIGHUP) }
