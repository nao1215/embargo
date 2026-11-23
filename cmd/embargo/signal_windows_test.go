//go:build windows

package main

import (
	"os"
	"testing"
)

// platformChild has no Windows-only child modes.
func platformChild(string) (int, bool) { return 0, false }

func TestWindowsKeepsAConsoleInterruptFromBeingPassedOn(t *testing.T) {
	t.Parallel()
	if openTerminal() != nil || !inForeground(nil) || !terminalSignal(os.Interrupt) || len(caughtSignals()) != 3 {
		t.Fatal("a console interrupt reaches the command already")
	}
}
