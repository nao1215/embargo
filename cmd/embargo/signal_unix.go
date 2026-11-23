//go:build !windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// caughtSignals are the signals embargo catches while the command runs: the
// ones a terminal, a container runtime or a service manager uses to stop,
// reload or reopen logs. A signal that is caught is not the Go default, so
// SIGQUIT no longer ends embargo with a goroutine dump.
func caughtSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}
}

// terminalSignal reports a signal that a terminal sends to its whole
// foreground process group, Ctrl-C and Ctrl-\, so the command may already
// have it.
func terminalSignal(received os.Signal) bool {
	return received == os.Interrupt || received == syscall.SIGQUIT
}

// openTerminal opens the controlling terminal, or returns nil when there is
// none, as in a container, a CI job or under a service manager.
func openTerminal() *os.File {
	terminal, err := os.Open("/dev/tty")
	if err != nil {
		return nil
	}
	return terminal
}

// inForeground reports whether this process's group is the foreground
// process group of terminal, so a Ctrl-C or Ctrl-\ typed there also reached
// the command, which runs in the same group. A nil terminal, or one that
// cannot be asked, is not in the foreground.
func inForeground(terminal *os.File) bool {
	if terminal == nil {
		return false
	}
	var group int32
	// #nosec G103 -- TIOCGPGRP writes the foreground process group id into group, which outlives the call.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, terminal.Fd(), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&group)))
	return errno == 0 && int(group) == syscall.Getpgrp()
}
