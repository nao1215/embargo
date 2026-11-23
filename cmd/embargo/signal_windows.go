//go:build windows

package main

import (
	"os"
	"syscall"
)

// caughtSignals are the signals embargo catches while the command runs.
// Windows can only end a process, so only an interrupt has an effect: it keeps
// embargo alive for the command's last output.
func caughtSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} }

// terminalSignal reports a signal the console sends to every process attached
// to it, so the command already has it.
func terminalSignal(received os.Signal) bool { return received == os.Interrupt }

// openTerminal returns nil; a console is not opened as a file.
func openTerminal() *os.File { return nil }

// inForeground reports true: Ctrl-C and Ctrl-Break reach every process
// attached to the console, embargo's command included.
func inForeground(*os.File) bool { return true }
