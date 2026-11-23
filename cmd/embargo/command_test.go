package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/nao1215/embargo"
)

// childArgument makes the test binary act as the command that run starts,
// so the tests need no shell and work on Windows too.
const childArgument = "embargo-test-child"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == childArgument {
		os.Exit(child(os.Args[2]))
	}
	os.Exit(m.Run())
}

// child writes a secret to each stream, or copies stdin, and returns a status.
func child(mode string) int {
	switch mode {
	case "leak":
		_, _ = os.Stdout.WriteString("out " + filterEmail + "\n") //nolint:errcheck // The test reads what arrived.
		_, _ = os.Stderr.WriteString("err " + filterPAN + "\n")   //nolint:errcheck // The test reads what arrived.
		return 3
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin) //nolint:errcheck // The test reads what arrived.
		return 0
	case "long":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", maximumLine+1) + "\n") //nolint:errcheck // The test reads what arrived.
		if _, err := os.Stdout.WriteString("after " + filterEmail + "\n"); err != nil {
			return 4
		}
		return 0
	case "term":
		process, _ := os.FindProcess(os.Getpid()) //nolint:errcheck // The own process is always found.
		_ = process.Signal(syscall.SIGTERM)       //nolint:errcheck // The test checks the status.
		return waitForSignal()
	case "wait":
		return waitForSignal()
	default:
		return 0
	}
}

// waitForSignal blocks until a signal ends the child, or a minute passes.
func waitForSignal() int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	<-ctx.Done()
	return 0
}

func runChild(t *testing.T, stdin, mode string) (code int, stdout, stderr string) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	code = exitCode([]string{runOperand, os.Args[0], childArgument, mode}, strings.NewReader(stdin), &out, &diagnostics)
	return code, out.String(), diagnostics.String()
}

func TestRunMasksBothStreamsAndKeepsTheStatus(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runChild(t, "", "leak")
	if code != 3 || stdout != "out "+filterMarker+"\n" || stderr != "err "+filterMarker+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runChild(t, "in "+filterEmail+"\n", "echo")
	if code != 0 || stdout != "in "+filterMarker+"\n" {
		t.Fatalf("stdin: code=%d stdout=%q", code, stdout)
	}
	var out bytes.Buffer
	if code := exitCode([]string{flagKinds, "pan", runOperand, "--", os.Args[0], childArgument, "leak"}, strings.NewReader(""), &out, io.Discard); code != 3 || out.String() != "out "+filterEmail+"\n" {
		t.Fatalf("flags before run: code=%d stdout=%q", code, out.String())
	}
}

func TestRunWithholdsAnOverLongLineAndKeepsTheCommandRunning(t *testing.T) {
	t.Parallel()
	// The command writes after its over-long line and fails with 4 when that
	// write is refused, so a command ended by a closed pipe cannot pass.
	code, stdout, stderr := runChild(t, "", "long")
	const notice = "embargo: line 1 exceeds the 4 MiB limit and was withheld\n"
	if code != statusFailed || stdout != notice+"after "+filterMarker+"\n" || stderr != notice {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunReportsASignalAsAShellDoes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no signals")
	}
	if code, _, _ := runChild(t, "", "term"); code != signalBase+int(syscall.SIGTERM) {
		t.Fatalf("code=%d", code)
	}
}

func TestRelaySignalsPassesTermButNotInterrupt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no signals")
	}
	waiting := exec.CommandContext(t.Context(), os.Args[0], childArgument, "wait") // #nosec G204 G702 -- The test binary runs itself.
	if err := waiting.Start(); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 2)
	// A passed-on interrupt would end the child first, with SIGINT.
	signals <- os.Interrupt
	signals <- syscall.SIGTERM
	close(signals)
	relaySignals(waiting.Process, signals)
	exit, ok := errors.AsType[*exec.ExitError](waiting.Wait())
	if !ok {
		t.Fatal("the child was not ended by a signal")
	}
	if status, isWait := exit.Sys().(syscall.WaitStatus); !isWait || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("child status = %v", exit)
	}
}

func TestRunCommandThatCannotStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		want string
		args []string
		code int
	}{
		{args: []string{runOperand, "embargo-no-such-command"}, code: statusNotFound, want: "embargo: the command was not found\n"},
		{args: []string{runOperand}, code: statusUsage, want: "embargo: run needs a command; see embargo --help\n"},
		{args: []string{runOperand, "--"}, code: statusUsage, want: "embargo: run needs a command; see embargo --help\n"},
		{args: []string{flagFormat, formatJSON, runOperand, "x"}, code: statusUsage, want: "embargo: run writes text only; --format cannot be used with it\n"},
	} {
		var out, diagnostics bytes.Buffer
		if code := exitCode(tc.args, strings.NewReader(""), &out, &diagnostics); code != tc.code || out.Len() != 0 || diagnostics.String() != tc.want {
			t.Errorf("%v: code=%d stderr=%q", tc.args, code, diagnostics.String())
		}
	}
	parsed, err := parseOptions([]string{"--", runOperand})
	if err != nil || parsed.command != nil || strings.Join(parsed.files, ",") != runOperand {
		t.Fatalf("-- run must read a file named run: %+v, %v", parsed, err)
	}
}

func TestStartFailureAndStatusMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
		code int
	}{
		{err: os.ErrPermission, code: statusCannotRun, want: "embargo: the command cannot be run: permission denied"},
		{err: errors.New("private-error"), code: statusCannotRun, want: "embargo: the command cannot be run"},
	} {
		status, ok := errors.AsType[statusError](startFailure(tc.err))
		if !ok || status.code != tc.code || status.Error() != tc.want {
			t.Errorf("%v: %+v", tc.err, status)
		}
	}
	if (statusError{cause: nil, code: 4}).Error() != "embargo: the command failed" {
		t.Error("an exit status without a cause has no text")
	}
	if status, ok := errors.AsType[statusError](commandStatus(errors.New("wait failed"), nil)); !ok || status.code != statusFailed {
		t.Errorf("a failed wait: %+v", status)
	}
	if !errors.Is(commandStatus(nil, []error{nil, errOutputClosed}), errOutputClosed) {
		t.Error("a closed output is not reported quietly")
	}
	if commandStatus(nil, []error{nil, nil}) != nil {
		t.Error("a successful command failed")
	}
}

func TestClosedOutputEndsQuietly(t *testing.T) {
	t.Parallel()
	var diagnostics bytes.Buffer
	if code := exitCode(nil, strings.NewReader("line\n"), closedOutput{}, &diagnostics); code != statusClosedOutput || diagnostics.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, diagnostics.String())
	}
	if !outputClosed(syscall.EPIPE) || outputClosed(errors.New("other")) {
		t.Fatal("outputClosed misreads EPIPE")
	}
	if err := writeFailed(errors.New("private-error")); err.Error() != outputWriteFailed {
		t.Fatalf("writeFailed = %v", err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], childArgument, "none") // #nosec G204 G702 -- The test binary runs itself.
	pipes, err := commandStreams(cmd)
	if err != nil || cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatalf("commandStreams: %v", err)
	}
	pipes.close()
}

// closedOutput fails every write as a pipe whose reader has gone.
type closedOutput struct{}

func (closedOutput) Write([]byte) (int, error) { return 0, syscall.EPIPE }

func TestWithheldLinesKeepTheStreamGoing(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maximumLine+1)
	limit := strings.Repeat("y", maximumLine-1) + "\n"
	for _, tc := range []struct {
		name, input, output, failure string
	}{
		{
			name:    "a line over the limit becomes a notice and the next line is masked",
			input:   "one " + filterEmail + "\n" + long + "\ntwo " + filterEmail + "\n",
			output:  "one " + filterMarker + "\nembargo: line 2 exceeds the 4 MiB limit and was withheld\ntwo " + filterMarker + "\n",
			failure: "embargo: line 2 exceeds the 4 MiB limit and was withheld",
		},
		{
			name:    "only the first withheld line is reported at the end",
			input:   long + "\n" + long + "\nlast\n",
			output:  "embargo: line 1 exceeds the 4 MiB limit and was withheld\nembargo: line 2 exceeds the 4 MiB limit and was withheld\nlast\n",
			failure: "embargo: line 1 exceeds the 4 MiB limit and was withheld",
		},
		{
			name:    "an unterminated over-long last line ends the stream with its notice",
			input:   "one\n" + long,
			output:  "one\nembargo: line 2 exceeds the 4 MiB limit and was withheld\n",
			failure: "embargo: line 2 exceeds the 4 MiB limit and was withheld",
		},
		{
			name:    "a line that runs a whole read buffer past the limit is skipped to its newline",
			input:   strings.Repeat("z", maximumLine+2*readBuffer) + "\nnext " + filterEmail + "\n",
			output:  "embargo: line 1 exceeds the 4 MiB limit and was withheld\nnext " + filterMarker + "\n",
			failure: "embargo: line 1 exceeds the 4 MiB limit and was withheld",
		},
		{
			name:   "a line of exactly 4 MiB with its newline is kept",
			input:  limit + "end\n",
			output: limit + "end\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := filterCommandOutput(strings.NewReader(tc.input), &out, newLineScanner(embargo.NewScanner(), inputLines))
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.failure {
				t.Fatalf("failure = %q, want %q", got, tc.failure)
			}
			if out.String() != tc.output {
				t.Fatalf("output (%d bytes) = %.200q, want %.200q", out.Len(), out.String(), tc.output)
			}
		})
	}
}

func TestWithheldLineReadFailureStopsTheStream(t *testing.T) {
	t.Parallel()
	input := io.MultiReader(strings.NewReader("one\n"+strings.Repeat("x", maximumLine+1)), iotest.ErrReader(errors.New("private-error")))
	var out bytes.Buffer
	err := filterCommandOutput(input, &out, newLineScanner(embargo.NewScanner(), inputLines))
	if err == nil || err.Error() != readFailedAtLine2 || out.String() != "one\n" {
		t.Fatalf("err=%v output=%.100q", err, out.String())
	}
}
