// Command agtrack tracks contexts, projects, tasks, subtasks, specs and the
// worklog in one SQLite database shared by every agent on the machine.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/l4sh/agtrack/internal/cli"
	"github.com/l4sh/agtrack/internal/errs"
)

func main() {
	// Deliver EPIPE as a write error instead of dying on SIGPIPE, so
	// `agtrack log ls | head` exits 0 (go spec §7.2).
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupts
		os.Exit(errs.ExitSIGINT)
	}()
	os.Exit(cli.Main())
}
