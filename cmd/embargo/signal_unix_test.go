//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// platformChild runs the child modes that ask about the controlling terminal.
func platformChild(mode string) (int, bool) {
	switch mode {
	case "foreground":
		if inForeground(openTerminal()) {
			return 0, true
		}
		return 5, true
	case "no-terminal":
		if openTerminal() == nil {
			return 0, true
		}
		return 6, true
	case "interrupt-parent":
		// The interrupt goes to embargo alone, as a container runtime sends
		// it; a passed-on interrupt ends this child before the sleep does.
		parent, _ := os.FindProcess(os.Getppid()) //nolint:errcheck // A process is always found on Unix.
		if err := parent.Signal(os.Interrupt); err != nil {
			return 8, true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		<-ctx.Done()
		return 7, true
	default:
		return 0, false
	}
}

// signalRecorder records the signals passed on instead of sending them.
type signalRecorder struct{ received []os.Signal }

func (r *signalRecorder) Signal(sig os.Signal) error {
	r.received = append(r.received, sig)
	return nil
}

func TestRelaySignalsPassesTerminalSignalsOnlyWithoutTheTerminal(t *testing.T) {
	t.Parallel()
	sent := []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}
	for _, tc := range []struct {
		name         string
		want         []os.Signal
		fromTerminal bool
	}{
		{name: "in the terminal's foreground Ctrl-C and Ctrl-\\ already reached the command", fromTerminal: true, want: []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}},
		{name: "without a terminal, as in a container, every signal is passed on", fromTerminal: false, want: sent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signals := make(chan os.Signal, len(sent))
			for _, sig := range sent {
				signals <- sig
			}
			close(signals)
			recorder := &signalRecorder{}
			relaySignals(recorder, signals, func() bool { return tc.fromTerminal })
			if !slices.Equal(recorder.received, tc.want) {
				t.Fatalf("passed on %v, want %v", recorder.received, tc.want)
			}
		})
	}
}

// The interrupt reaches every run in this test binary that is waiting for
// signals, so this test runs before the parallel ones start.
//
//nolint:paralleltest // An interrupt sent to the test process must reach only this run.
func TestRunPassesAnInterruptOnWithoutTheTerminal(t *testing.T) {
	// A test run from a terminal is in its foreground group, where a typed
	// Ctrl-C reaches the command already and is not passed on again.
	want := signalBase + int(syscall.SIGINT)
	if inForeground(openTerminal()) {
		want = 7
	}
	if code, _, _ := runChild(t, "", "interrupt-parent"); code != want {
		t.Fatalf("code=%d, want %d", code, want)
	}
}

func TestCaughtSignalsAreTheStopReloadAndTerminalSignals(t *testing.T) {
	t.Parallel()
	want := []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}
	if got := caughtSignals(); !slices.Equal(got, want) {
		t.Fatalf("caught %v, want %v", got, want)
	}
	var terminal []os.Signal
	for _, sig := range want {
		if terminalSignal(sig) {
			terminal = append(terminal, sig)
		}
	}
	if !slices.Equal(terminal, []os.Signal{os.Interrupt, syscall.SIGQUIT}) {
		t.Fatalf("terminal signals %v", terminal)
	}
}

func TestInForegroundOnlyForTheTerminalsForegroundGroup(t *testing.T) {
	t.Parallel()
	if inForeground(nil) {
		t.Error("no terminal is in the foreground")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = read.Close(), write.Close() }() //nolint:errcheck // The pipe was only asked about.
	if inForeground(read) {
		t.Error("a pipe is not a terminal")
	}
	// A child in a session of its own has no controlling terminal.
	alone := exec.CommandContext(t.Context(), os.Args[0], childArgument, "no-terminal") // #nosec G204 G702 -- The test binary runs itself.
	alone.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := alone.Run(); err != nil {
		t.Errorf("a new session opened a terminal: %v", err)
	}
	if runtime.GOOS != "linux" {
		return
	}
	// A child that leads a session on a pseudo-terminal is its foreground.
	terminal := openPseudoTerminal(t)
	leader := exec.CommandContext(t.Context(), os.Args[0], childArgument, "foreground") // #nosec G204 G702 -- The test binary runs itself.
	leader.Stdin = terminal
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := leader.Run(); err != nil {
		t.Errorf("the session leader is not the foreground of its terminal: %v", err)
	}
}

// openPseudoTerminal opens the follower side of a new Linux pseudo-terminal
// without making it this process's controlling terminal.
func openPseudoTerminal(t *testing.T) *os.File {
	t.Helper()
	const (
		setLock   = 0x40045431 // TIOCSPTLCK
		getNumber = 0x80045430 // TIOCGPTN
	)
	leader, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { _ = leader.Close() }) //nolint:errcheck // The terminal is only used by the test.
	var unlock int32
	var number uint32
	// #nosec G103 -- TIOCSPTLCK reads unlock, which outlives the call.
	if errno := ioctl(leader, setLock, unsafe.Pointer(&unlock)); errno != 0 {
		t.Fatalf("unlock the pseudo-terminal: %v", errno)
	}
	// #nosec G103 -- TIOCGPTN writes the terminal number into number, which outlives the call.
	if errno := ioctl(leader, getNumber, unsafe.Pointer(&number)); errno != 0 {
		t.Fatalf("number of the pseudo-terminal: %v", errno)
	}
	follower, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Close() }) //nolint:errcheck // The terminal is only used by the test.
	return follower
}

// ioctl issues request on file with value, as a C ioctl call does.
func ioctl(file *os.File, request uintptr, value unsafe.Pointer) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), request, uintptr(value))
	return errno
}
