package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *eventLog) add(event string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
}

func (log *eventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

type eventWriter struct {
	log *eventLog
	buf lockedBuffer
}

func (writer *eventWriter) Write(data []byte) (int, error) {
	writer.log.add("child-output")
	return writer.buf.Write(data)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.String()
}

func TestHandoffLeavesAndRestoresAroundExclusivePTY(t *testing.T) {
	log := &eventLog{}
	output := &eventWriter{log: log}
	handoff := Handoff{
		Input:  strings.NewReader("private-input\n"),
		Output: output,
		State: StateFuncs{
			ReleaseFunc: func() error { log.add("release"); return nil },
			RestoreFunc: func() error { log.add("restore"); return nil },
		},
	}
	exit, err := handoff.Run(context.Background(), exec.Command("/bin/sh", "-c", "IFS= read -r line; printf 'child:%s' \"$line\""))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if exit.ExitCode != 0 || exit.Signal != 0 {
		t.Fatalf("exit = %#v, want code 0", exit)
	}
	if got := output.buf.String(); !strings.Contains(got, "child:private-input") {
		t.Fatalf("direct PTY output = %q", got)
	}
	events := log.snapshot()
	if len(events) < 3 || events[0] != "release" || events[len(events)-1] != "restore" {
		t.Fatalf("terminal ordering = %v", events)
	}
	for i, event := range events {
		if event == "child-output" && (i == 0 || i == len(events)-1) {
			t.Fatalf("child output occurred outside release/restore: %v", events)
		}
	}
}

func TestResizeForwardedToRealPTY(t *testing.T) {
	resize := make(chan WindowSize, 1)
	var output lockedBuffer
	handoff := Handoff{
		Output:      &output,
		InitialSize: WindowSize{Width: 40, Height: 11},
		Resize:      resize,
	}
	command := exec.Command("/bin/sh", "-c", "stty size; trap 'stty size; exit 0' WINCH; echo ready; while :; do sleep 0.02; done")
	result := make(chan error, 1)
	go func() {
		exit, err := handoff.Run(context.Background(), command)
		if err == nil && exit.ExitCode != 0 {
			err = errors.New("resize child did not exit successfully")
		}
		result <- err
	}()
	waitForOutput(t, &output, "ready")
	resize <- WindowSize{Width: 91, Height: 27}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resize did not wake child")
	}
	got := strings.ReplaceAll(output.String(), "\r", "")
	if !strings.Contains(got, "11 40") || !strings.Contains(got, "27 91") {
		t.Fatalf("PTY sizes = %q, want initial and forwarded dimensions", got)
	}
}

func TestWatchResizeForwardsHostSIGWINCH(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(master, &pty.Winsize{Cols: 40, Rows: 11}); err != nil {
		t.Fatal(err)
	}
	initial, events, stop := WatchResize(nil, master)
	defer stop()
	if initial != (WindowSize{Width: 40, Height: 11}) || events == nil {
		t.Fatalf("WatchResize() initial = %#v, events nil = %t", initial, events == nil)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: 91, Rows: 27}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-events:
		if size != (WindowSize{Width: 91, Height: 27}) {
			t.Fatalf("resize = %#v", size)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host SIGWINCH was not forwarded")
	}
}

func TestHandoffForwardsConfiguredHostSignals(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "interrupt", signal: syscall.SIGINT},
		{name: "terminate", signal: syscall.SIGTERM},
		{name: "quit", signal: syscall.SIGQUIT},
	} {
		t.Run(test.name, func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			var output lockedBuffer
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan struct {
				exit Exit
				err  error
			}, 1)
			go func() {
				exit, err := (Handoff{Output: &output, Signals: signals}).Run(ctx, exec.Command("/bin/sh", "-c", "trap 'exit 42' INT TERM QUIT; echo ready; while :; do sleep 0.02; done"))
				result <- struct {
					exit Exit
					err  error
				}{exit: exit, err: err}
			}()
			waitForOutput(t, &output, "ready")
			signals <- test.signal
			got := <-result
			if got.err != nil || got.exit.ExitCode != 42 || got.exit.Signal != 0 {
				t.Fatalf("Run() = %#v, %v; want trapped signal exit 42", got.exit, got.err)
			}
		})
	}
}

func TestHandoffSecondInterruptEscalatesWithoutDuplicateForward(t *testing.T) {
	signals := make(chan os.Signal, 2)
	var output lockedBuffer
	var restores int
	result := make(chan struct {
		exit Exit
		err  error
	}, 1)
	go func() {
		exit, err := (Handoff{
			Output:  &output,
			Signals: signals,
			State: StateFuncs{RestoreFunc: func() error {
				restores++
				return nil
			}},
		}).Run(context.Background(), exec.Command("/bin/sh", "-c", "trap 'echo forwarded' INT TERM; echo ready; while :; do sleep 0.02; done"))
		result <- struct {
			exit Exit
			err  error
		}{exit: exit, err: err}
	}()

	waitForOutput(t, &output, "ready")
	signals <- syscall.SIGINT
	waitForOutput(t, &output, "forwarded")
	time.Sleep(100 * time.Millisecond)
	if count := strings.Count(output.String(), "forwarded"); count != 1 {
		t.Fatalf("first interrupt forwarded %d times, output = %q", count, output.String())
	}
	signals <- syscall.SIGTERM
	select {
	case got := <-result:
		if got.err != nil || got.exit.ExitCode != -1 || got.exit.Signal != syscall.SIGKILL {
			t.Fatalf("second interrupt Run() = %#v, %v; want SIGKILL", got.exit, got.err)
		}
		if restores != 1 {
			t.Fatalf("terminal restores = %d, want 1", restores)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second interrupt did not escalate")
	}
}

func TestCommandSignalContextCancelsNoninteractiveAndYieldsInteractiveOwnership(t *testing.T) {
	t.Run("noninteractive cancellation", func(t *testing.T) {
		ctx, stop := CommandSignalContext(context.Background())
		defer stop()
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("context error = %v, want canceled", ctx.Err())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("noninteractive signal did not cancel command context")
		}
	})

	t.Run("interactive ownership", func(t *testing.T) {
		ctx, stop := CommandSignalContext(context.Background())
		defer stop()
		claimed, signals, ok := ClaimInteractiveSignalOwnership(ctx)
		if !ok {
			t.Fatal("command signal context was not claimable")
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case signal := <-signals:
			if signal != syscall.SIGTERM {
				t.Fatalf("interactive signal = %v, want SIGTERM", signal)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("interactive watcher did not receive host signal")
		}
		select {
		case <-claimed.Done():
			t.Fatalf("interactive context canceled: %v", claimed.Err())
		case <-time.After(50 * time.Millisecond):
		}
		select {
		case duplicate := <-signals:
			t.Fatalf("interactive watcher received duplicate signal %v", duplicate)
		default:
		}
	})

	t.Run("claimed context preserves child cancellation", func(t *testing.T) {
		ctx, stop := CommandSignalContext(context.Background())
		defer stop()
		childCtx, cancelChild := context.WithCancel(ctx)
		claimed, _, ok := ClaimInteractiveSignalOwnership(childCtx)
		if !ok {
			t.Fatal("derived command signal context was not claimable")
		}
		cancelChild()
		select {
		case <-claimed.Done():
			if !errors.Is(claimed.Err(), context.Canceled) {
				t.Fatalf("claimed context error = %v", claimed.Err())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("claimed context lost child cancellation")
		}
	})

	t.Run("pending quit transfers exactly once", func(t *testing.T) {
		ctx, stop := CommandSignalContext(context.Background())
		defer stop()
		if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("pending SIGQUIT did not cancel setup")
		}
		claimed, signals, ok := ClaimInteractiveSignalOwnership(ctx)
		if !ok {
			t.Fatal("command signal context was not claimable")
		}
		select {
		case received := <-signals:
			if received != syscall.SIGQUIT {
				t.Fatalf("pending signal = %v, want SIGQUIT", received)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("pending SIGQUIT was not transferred")
		}
		select {
		case duplicate := <-signals:
			t.Fatalf("pending SIGQUIT transferred twice: %v", duplicate)
		default:
		}
		select {
		case <-claimed.Done():
			t.Fatalf("claimed child context remained canceled: %v", claimed.Err())
		default:
		}
	})
}

func TestPendingCommandSignalAtHandoffReachesChildOnceAndRestores(t *testing.T) {
	ctx, stop := CommandSignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("SIGQUIT did not become pending before handoff")
	}
	handoffCtx, signals, ok := ClaimInteractiveSignalOwnership(ctx)
	if !ok {
		t.Fatal("command signal context was not claimable")
	}
	handoffCtx, cancel := context.WithTimeout(handoffCtx, 3*time.Second)
	defer cancel()
	restores := 0
	exit, err := (Handoff{
		Output:  io.Discard,
		Signals: signals,
		State: StateFuncs{RestoreFunc: func() error {
			restores++
			return nil
		}},
	}).Run(handoffCtx, exec.Command("/bin/sh", "-c", "while :; do :; done"))
	if err != nil || exit.Signal != syscall.SIGQUIT || exit.ExitCode != -1 {
		t.Fatalf("pending SIGQUIT handoff = %#v, %v", exit, err)
	}
	if restores != 1 {
		t.Fatalf("terminal restores = %d, want 1", restores)
	}
	select {
	case duplicate := <-signals:
		t.Fatalf("pending signal remained queued twice: %v", duplicate)
	default:
	}
}

func TestHandoffExactExitSignalCancellationAndFailureRestore(t *testing.T) {
	t.Run("exit code", func(t *testing.T) {
		exit, err := (Handoff{}).Run(context.Background(), exec.Command("/bin/sh", "-c", "exit 37"))
		if err != nil || exit.ExitCode != 37 || exit.Signal != 0 {
			t.Fatalf("Run() = %#v, %v; want exit 37", exit, err)
		}
	})
	t.Run("signal", func(t *testing.T) {
		exit, err := (Handoff{}).Run(context.Background(), exec.Command("/bin/sh", "-c", "kill -TERM $$"))
		if err != nil || exit.ExitCode != -1 || exit.Signal != syscall.SIGTERM {
			t.Fatalf("Run() = %#v, %v; want SIGTERM", exit, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var output lockedBuffer
		restored := false
		result := make(chan struct {
			exit Exit
			err  error
		}, 1)
		go func() {
			exit, err := (Handoff{Output: &output, CancelGrace: time.Second, State: StateFuncs{RestoreFunc: func() error { restored = true; return nil }}}).Run(ctx, exec.Command("/bin/sh", "-c", "trap 'exit 23' TERM; echo ready; while :; do sleep 0.02; done"))
			result <- struct {
				exit Exit
				err  error
			}{exit, err}
		}()
		waitForOutput(t, &output, "ready")
		cancel()
		select {
		case got := <-result:
			if !errors.Is(got.err, context.Canceled) || got.exit.ExitCode != 23 {
				t.Fatalf("cancel Run() = %#v, %v; want context canceled and exit 23", got.exit, got.err)
			}
			if !restored {
				t.Fatal("terminal not restored after cancellation")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled child did not exit")
		}
	})
	t.Run("start failure", func(t *testing.T) {
		var events []string
		state := StateFuncs{
			ReleaseFunc: func() error { events = append(events, "release"); return nil },
			RestoreFunc: func() error { events = append(events, "restore"); return nil },
		}
		_, err := (Handoff{State: state}).Run(context.Background(), exec.Command("/definitely/not/a/program"))
		if err == nil || strings.Join(events, ",") != "release,restore" {
			t.Fatalf("start failure error = %v, events = %v", err, events)
		}
	})
}

func TestHandoffBoundsDrainWhenBackgroundProcessKeepsPTYOpen(t *testing.T) {
	restored := false
	started := time.Now()
	exit, err := (Handoff{
		Output:      io.Discard,
		OutputDrain: 50 * time.Millisecond,
		State: StateFuncs{RestoreFunc: func() error {
			restored = true
			return nil
		}},
	}).Run(context.Background(), exec.Command("/bin/sh", "-c", "trap '' HUP; (trap '' HUP; sleep 5) & exit 0"))
	if err != nil || exit.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", exit, err)
	}
	if !restored || time.Since(started) > time.Second {
		t.Fatalf("restore=%t duration=%s", restored, time.Since(started))
	}
}

func TestInputPumpStopsBeforeRestoreAcrossRepeatedHandoffs(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create open terminal input pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	baseline := runtime.NumGoroutine()
	for iteration := range 24 {
		marker := byte(iteration + 1)
		result := make(chan error, 1)
		go func() {
			exit, runErr := (Handoff{
				Input: reader,
				State: StateFuncs{RestoreFunc: func() error {
					_, writeErr := writer.Write([]byte{marker})
					return writeErr
				}},
			}).Run(context.Background(), exec.Command("/bin/sh", "-c", "exit 0"))
			if runErr == nil && exit.ExitCode != 0 {
				runErr = fmt.Errorf("child exit = %#v", exit)
			}
			result <- runErr
		}()
		select {
		case runErr := <-result:
			if runErr != nil {
				t.Fatalf("handoff %d: %v", iteration, runErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("handoff %d retained a blocked input copier", iteration)
		}

		if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set input deadline: %v", err)
		}
		var got [1]byte
		if _, err := io.ReadFull(reader, got[:]); err != nil {
			t.Fatalf("read restored input marker after handoff %d: %v", iteration, err)
		}
		if got[0] != marker {
			t.Fatalf("restored input marker after handoff %d = %d, want %d", iteration, got[0], marker)
		}
		if err := reader.SetReadDeadline(time.Time{}); err != nil {
			t.Fatalf("clear input deadline: %v", err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutines grew from %d to %d after repeated handoffs", baseline, got)
	}
}

func waitForOutput(t *testing.T, output *lockedBuffer, substring string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), substring) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", substring, output.String())
}
