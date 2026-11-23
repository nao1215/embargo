package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/nao1215/embargo"
)

// Exit statuses for a command that cannot start, as a shell reports them.
const (
	statusCannotRun = 126
	statusNotFound  = 127
	signalBase      = 128
)

// statusError ends embargo with code. cause, when set, is printed first.
type statusError struct {
	cause error
	code  int
}

// Error returns the cause, or a fixed text when there is none.
func (e statusError) Error() string {
	if e.cause == nil {
		return "embargo: the command failed"
	}
	return e.cause.Error()
}

// runCommand starts argv with input and the environment, masks its standard
// output onto output and its standard error onto diagnostics, each line by
// line as the filter does, and returns its status. A command ended by a
// signal reports 128 plus the signal, as a shell does.
//
// The signals in caughtSignals that reach embargo are passed on, and embargo
// goes on masking until the command ends. Ctrl-C and Ctrl-\ typed at the
// terminal whose foreground group embargo is in already reach the command,
// which runs in the same group, so those are not passed on a second time;
// without such a terminal, as in a container, they are passed on too.
func runCommand(argv []string, scanner *embargo.Scanner, input io.Reader, output, diagnostics io.Writer) error {
	// #nosec G204 -- The user names the command to run on the command line.
	command := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	command.Stdin = input
	streams, err := commandStreams(command)
	if err != nil {
		return err
	}
	caught := caughtSignals()
	signals := make(chan os.Signal, len(caught))
	signal.Notify(signals, caught...)
	terminal := openTerminal()
	defer func() {
		signal.Stop(signals)
		close(signals)
		// A terminal that was never opened is nil, and closing it only returns an error.
		_ = terminal.Close() //nolint:errcheck // The terminal was only asked for its foreground group.
	}()
	if err := command.Start(); err != nil {
		streams.close()
		return startFailure(err)
	}
	streams.closeChildEnds()
	go relaySignals(command.Process, signals, func() bool { return inForeground(terminal) })
	failures := streams.mask(scanner, output, diagnostics)
	return commandStatus(command.Wait(), failures)
}

// signalTarget receives the signals passed on: the command's process.
type signalTarget interface {
	Signal(sig os.Signal) error
}

// relaySignals passes each signal on to the command, except a terminal
// signal while fromTerminal reports that the terminal sent it to the command
// as well; that one only keeps embargo from ending.
func relaySignals(process signalTarget, signals <-chan os.Signal, fromTerminal func() bool) {
	for received := range signals {
		if terminalSignal(received) && fromTerminal() {
			continue
		}
		_ = process.Signal(received) //nolint:errcheck // A command that has already ended needs no signal.
	}
}

// startFailure maps a command that cannot start to 127 when it is not found
// and 126 otherwise, without repeating its name.
func startFailure(err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return statusError{cause: errors.New("embargo: the command was not found"), code: statusNotFound}
	case errors.Is(err, fs.ErrPermission):
		return statusError{cause: errors.New("embargo: the command cannot be run: permission denied"), code: statusCannotRun}
	default:
		return statusError{cause: errors.New("embargo: the command cannot be run"), code: statusCannotRun}
	}
}

// commandStatus returns the command's status. A masking failure, such as an
// over-long line, is reported and turns a successful status into 1; a closed
// output ends quietly as the filter does.
func commandStatus(waitErr error, failures []error) error {
	code := 0
	if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		code = exit.ExitCode()
		if status, isWait := exit.Sys().(syscall.WaitStatus); isWait && status.Signaled() {
			code = signalBase + int(status.Signal())
		}
	} else if waitErr != nil {
		code = statusFailed
	}
	for _, failure := range failures {
		if errors.Is(failure, errOutputClosed) {
			return failure
		}
	}
	if failure := errors.Join(failures...); failure != nil {
		return statusError{cause: failure, code: max(code, statusFailed)}
	}
	if code != 0 {
		return statusError{cause: nil, code: code}
	}
	return nil
}

// commandPipes holds the read and write ends for the command's standard
// output and standard error.
type commandPipes struct {
	read, write [2]*os.File
}

// commandStreams connects the command's standard output and standard error
// to new pipes. The command writes to the pipes directly, and a process it
// leaves behind keeps them open, as it would keep a shell pipe open.
func commandStreams(command *exec.Cmd) (*commandPipes, error) {
	pipes := &commandPipes{}
	for i := range pipes.read {
		read, write, err := os.Pipe()
		if err != nil {
			pipes.close()
			return nil, errors.New("embargo: the command cannot be run: no pipe")
		}
		pipes.read[i], pipes.write[i] = read, write
	}
	command.Stdout, command.Stderr = pipes.write[0], pipes.write[1]
	return pipes, nil
}

// closeChildEnds closes this process's copies of the write ends once the
// command holds its own, so each read ends when the command closes them.
// A pipe end that was never made is nil, and closing it only returns an error.
func (p *commandPipes) closeChildEnds() {
	for _, write := range p.write {
		_ = write.Close() //nolint:errcheck // The command holds its own copy; nothing was written here.
	}
}

func (p *commandPipes) close() {
	p.closeChildEnds()
	for _, read := range p.read {
		_ = read.Close() //nolint:errcheck // Nothing is read from a pipe that is closed unused.
	}
}

// filterCommandOutput masks one stream of the command as filterLines does,
// except that a line over the limit is replaced with a notice naming only its
// number and the stream goes on, so the command is not ended by a closed pipe
// because of one long line. The first withheld line is returned at the end.
func filterCommandOutput(input io.Reader, output io.Writer, lines *lineScanner) error {
	writer := bufio.NewWriterSize(output, ioBuffer)
	masked := &pipeline{base: lines, render: renderMasked, consequence: outputStops, withhold: true, lines: 0, size: 0}
	var withheld error
	err := masked.run(input, func(b *batch) error {
		withheld = cmp.Or(withheld, b.withheld)
		return writeBatch(writer, b)
	})
	return cmp.Or(err, withheld)
}

// mask filters both streams at once and returns their failures. A stream
// that stops early is closed, so the command gets a broken pipe instead of
// blocking on a pipe nobody reads.
func (p *commandPipes) mask(scanner *embargo.Scanner, output, diagnostics io.Writer) []error {
	failures := make([]error, len(p.read))
	var group sync.WaitGroup
	for i, destination := range []io.Writer{output, diagnostics} {
		group.Go(func() {
			failures[i] = filterCommandOutput(p.read[i], destination, newLineScanner(scanner, inputLines))
			_ = p.read[i].Close() //nolint:errcheck // The stream was read to its end or abandoned.
		})
	}
	group.Wait()
	return failures
}
