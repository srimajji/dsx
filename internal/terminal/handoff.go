package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
)

// WindowSize is a terminal size in character cells.
type WindowSize struct {
	Width  uint16
	Height uint16
}

// TerminalState releases Bubble Tea/raw/alternate-screen state before a child
// owns the terminal and restores it after the child exits. Implementations must
// be safe to call Restore after a partial Release failure.
type TerminalState interface {
	Release() error
	Restore() error
}

// StateFuncs adapts lifecycle callbacks into TerminalState.
type StateFuncs struct {
	ReleaseFunc func() error
	RestoreFunc func() error
}

func (state StateFuncs) Release() error {
	if state.ReleaseFunc == nil {
		return nil
	}
	return state.ReleaseFunc()
}

func (state StateFuncs) Restore() error {
	if state.RestoreFunc == nil {
		return nil
	}
	return state.RestoreFunc()
}

// Exit describes the exact child process outcome. ExitCode is -1 when the
// process ended due to a signal; Signal is zero for an ordinary exit.
type Exit struct {
	ExitCode int
	Signal   syscall.Signal
}

// Handoff gives a real PTY child exclusive access to terminal input and output.
// Child bytes are copied directly to Output and are never returned for model
// rendering. Resize changes and configured host signals are forwarded to the
// child's process group until it exits.
type Handoff struct {
	Input       io.Reader
	Output      io.Writer
	State       TerminalState
	InitialSize WindowSize
	Resize      <-chan WindowSize
	Signals     <-chan os.Signal
	CancelGrace time.Duration
	OutputDrain time.Duration
}

// Run releases terminal UI state, runs command on a new PTY session, and
// restores terminal UI state on every return path. Context cancellation sends
// SIGTERM to the child's process group, escalates to SIGKILL after CancelGrace,
// and returns the context error while preserving the exact child Exit outcome.
func (handoff Handoff) Run(ctx context.Context, command *exec.Cmd) (exit Exit, err error) {
	exit.ExitCode = -1
	if ctx == nil {
		ctx = context.Background()
	}
	if command == nil {
		return exit, errors.New("interactive child command is nil")
	}
	if handoff.State == nil {
		handoff.State = StateFuncs{}
	}

	released := false
	defer func() {
		restoreErr := handoff.State.Restore()
		if restoreErr != nil {
			if released {
				err = errors.Join(err, fmt.Errorf("restore terminal after interactive child: %w", restoreErr))
			} else {
				err = errors.Join(err, fmt.Errorf("restore terminal after failed release: %w", restoreErr))
			}
		}
	}()
	if releaseErr := handoff.State.Release(); releaseErr != nil {
		return exit, fmt.Errorf("release terminal for interactive child: %w", releaseErr)
	}
	released = true

	size := handoff.InitialSize
	if size.Width == 0 {
		size.Width = 80
	}
	if size.Height == 0 {
		size.Height = 24
	}
	var input cancelreader.CancelReader
	if handoff.Input != nil {
		prepared, prepareErr := cancelreader.NewReader(handoff.Input)
		if prepareErr != nil {
			return exit, fmt.Errorf("prepare cancellable terminal input: %w", prepareErr)
		}
		input = prepared
	}
	master, startErr := pty.StartWithSize(command, &pty.Winsize{Cols: size.Width, Rows: size.Height})
	if startErr != nil {
		if input != nil {
			_ = input.Close()
		}
		return exit, fmt.Errorf("start interactive child PTY: %w", startErr)
	}
	defer master.Close()

	outputDone := make(chan error, 1)
	output := handoff.Output
	if output == nil {
		output = io.Discard
	}
	go func() {
		_, copyErr := io.Copy(output, master)
		if errors.Is(copyErr, os.ErrClosed) || errors.Is(copyErr, syscall.EIO) {
			copyErr = nil
		}
		outputDone <- copyErr
	}()

	var inputDone chan error
	if input != nil {
		inputDone = make(chan error, 1)
		go func() {
			_, copyErr := io.Copy(master, input)
			if errors.Is(copyErr, cancelreader.ErrCanceled) || errors.Is(copyErr, os.ErrClosed) || errors.Is(copyErr, syscall.EIO) {
				copyErr = nil
			}
			inputDone <- copyErr
		}()
	}

	stopResize := make(chan struct{})
	var resizeWG sync.WaitGroup
	var resizeErrCh chan error
	if handoff.Resize != nil {
		resizeErrCh = make(chan error, 1)
		resizeWG.Add(1)
		go func() {
			defer resizeWG.Done()
			for {
				select {
				case next, ok := <-handoff.Resize:
					if !ok {
						return
					}
					if next.Width == 0 || next.Height == 0 {
						continue
					}
					if resizeErr := pty.Setsize(master, &pty.Winsize{Cols: next.Width, Rows: next.Height}); resizeErr != nil {
						select {
						case resizeErrCh <- resizeErr:
						default:
						}
					}
				case <-stopResize:
					return
				}
			}
		}()
	}

	stopSignals := make(chan struct{})
	var signalWG sync.WaitGroup
	var signalErrCh chan error
	if handoff.Signals != nil {
		signalErrCh = make(chan error, 1)
		signalWG.Add(1)
		interrupted := false
		go func() {
			defer signalWG.Done()
			for {
				select {
				case next, ok := <-handoff.Signals:
					if !ok {
						return
					}
					hostSignal, ok := next.(syscall.Signal)
					if !ok {
						continue
					}
					forwardSignal := hostSignal
					if hostSignal == syscall.SIGINT || hostSignal == syscall.SIGTERM {
						if interrupted {
							forwardSignal = syscall.SIGKILL
						}
						interrupted = true
					}
					if signalErr := signalProcessGroup(command.Process, forwardSignal); signalErr != nil && !errors.Is(signalErr, syscall.ESRCH) {
						select {
						case signalErrCh <- signalErr:
						default:
						}
					}
				case <-stopSignals:
					return
				}
			}
		}()
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	waitErr := error(nil)
	cancelled := false
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		cancelled = true
		_ = signalProcessGroup(command.Process, syscall.SIGTERM)
		grace := handoff.CancelGrace
		if grace <= 0 {
			grace = 500 * time.Millisecond
		}
		timer := time.NewTimer(grace)
		select {
		case waitErr = <-waitCh:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = signalProcessGroup(command.Process, syscall.SIGKILL)
			waitErr = <-waitCh
		}
	}
	close(stopResize)
	resizeWG.Wait()
	close(stopSignals)
	signalWG.Wait()

	if input != nil {
		input.Cancel()
	}
	var inputErr error
	if inputDone != nil {
		inputErr = <-inputDone
	}
	var inputCloseErr error
	if input != nil {
		inputCloseErr = input.Close()
	}

	exit = processExit(command.ProcessState)
	drain := handoff.OutputDrain
	if drain <= 0 {
		drain = 250 * time.Millisecond
	}
	var copyErr error
	timer := time.NewTimer(drain)
	select {
	case copyErr = <-outputDone:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
		_ = master.Close()
		_ = signalProcessGroup(command.Process, syscall.SIGTERM)
		copyErr = <-outputDone
	}
	var resizeErr error
	if resizeErrCh != nil {
		select {
		case resizeErr = <-resizeErrCh:
		default:
		}
	}
	var signalErr error
	if signalErrCh != nil {
		select {
		case signalErr = <-signalErrCh:
		default:
		}
	}
	if cancelled {
		return exit, errors.Join(
			ctx.Err(),
			wrapPTYError("copy terminal input to interactive child", inputErr),
			wrapPTYError("close cancellable terminal input", inputCloseErr),
			wrapPTYError("copy interactive child output", copyErr),
			wrapPTYError("resize interactive child PTY", resizeErr),
			wrapPTYError("forward host signal to interactive child", signalErr),
		)
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return exit, errors.Join(
				fmt.Errorf("wait for interactive child: %w", waitErr),
				wrapPTYError("copy terminal input to interactive child", inputErr),
				wrapPTYError("close cancellable terminal input", inputCloseErr),
				wrapPTYError("copy interactive child output", copyErr),
				wrapPTYError("resize interactive child PTY", resizeErr),
				wrapPTYError("forward host signal to interactive child", signalErr),
			)
		}
	}
	return exit, errors.Join(
		wrapPTYError("copy terminal input to interactive child", inputErr),
		wrapPTYError("close cancellable terminal input", inputCloseErr),
		wrapPTYError("copy interactive child output", copyErr),
		wrapPTYError("resize interactive child PTY", resizeErr),
		wrapPTYError("forward host signal to interactive child", signalErr),
	)
}

func processExit(state *os.ProcessState) Exit {
	exit := Exit{ExitCode: -1}
	if state == nil {
		return exit
	}
	exit.ExitCode = state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exit.Signal = status.Signal()
	}
	return exit
}

func signalProcessGroup(process *os.Process, signal syscall.Signal) error {
	if process == nil {
		return nil
	}
	return syscall.Kill(-process.Pid, signal)
}

func wrapPTYError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
