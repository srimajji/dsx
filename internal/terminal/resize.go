package terminal

import (
	"context"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	xterm "github.com/charmbracelet/x/term"
)

type commandSignalOwnership struct {
	parent         context.Context
	cancel         context.CancelFunc
	hostSignals    chan os.Signal
	childSignals   chan os.Signal
	done           chan struct{}
	once           sync.Once
	mu             sync.RWMutex
	claimed        bool
	signalCanceled bool
}

type commandSignalOwnershipKey struct{}

// CommandSignalContext cancels ordinary host commands on terminal-directed
// signals. Once an interactive child claims ownership, the same watcher routes
// signals to that child instead, leaving its exit status authoritative.
func CommandSignalContext(parent context.Context) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	notified, cancel := context.WithCancel(parent)
	ownership := &commandSignalOwnership{
		parent:       parent,
		cancel:       cancel,
		hostSignals:  make(chan os.Signal, 8),
		childSignals: make(chan os.Signal, 8),
		done:         make(chan struct{}),
	}
	signal.Notify(ownership.hostSignals, syscall.SIGHUP, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	go ownership.watch()
	return context.WithValue(notified, commandSignalOwnershipKey{}, ownership), ownership.stop
}

func (ownership *commandSignalOwnership) watch() {
	for {
		select {
		case next := <-ownership.hostSignals:
			ownership.mu.Lock()
			claimed := ownership.claimed
			if !claimed {
				ownership.signalCanceled = true
			}
			ownership.mu.Unlock()
			if !claimed {
				ownership.cancel()
			}
			// Queue every observed signal for a possible handoff. If setup
			// honors cancellation there is no child; if a signal races with
			// ownership transfer, Handoff forwards the already queued signal
			// exactly once.
			select {
			case ownership.childSignals <- next:
			case <-ownership.done:
				return
			}
		case <-ownership.done:
			return
		}
	}
}

func (ownership *commandSignalOwnership) stop() {
	ownership.once.Do(func() {
		signal.Stop(ownership.hostSignals)
		close(ownership.done)
		ownership.cancel()
	})
}

// ClaimInteractiveSignalOwnership transfers CommandSignalContext's sole signal
// watcher to a child-facing channel. Contexts not created by
// CommandSignalContext are returned unchanged for callers to watch separately.
func ClaimInteractiveSignalOwnership(ctx context.Context) (context.Context, <-chan os.Signal, bool) {
	if ctx == nil {
		return context.Background(), nil, false
	}
	ownership, ok := ctx.Value(commandSignalOwnershipKey{}).(*commandSignalOwnership)
	if !ok || ownership == nil {
		return ctx, nil, false
	}
	ownership.mu.Lock()
	ownership.claimed = true
	signalCanceled := ownership.signalCanceled
	ownership.mu.Unlock()
	if signalCanceled && ownership.parent.Err() == nil {
		return ownership.parent, ownership.childSignals, true
	}
	return ctx, ownership.childSignals, true
}

// WatchResize snapshots a terminal's current size and forwards subsequent
// SIGWINCH changes until stop is called. Non-terminal streams return no events.
func WatchResize(input io.Reader, output io.Writer) (WindowSize, <-chan WindowSize, func()) {
	fd, found := terminalFD(output)
	initial, valid := WindowSize{}, false
	if found {
		initial, valid = readWindowSize(fd)
	}
	if !valid {
		fd, found = terminalFD(input)
		if found {
			initial, valid = readWindowSize(fd)
		}
	}
	if !valid {
		return WindowSize{}, nil, func() {}
	}

	signals := make(chan os.Signal, 1)
	events := make(chan WindowSize, 1)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-signals:
				next, valid := readWindowSize(fd)
				if !valid {
					continue
				}
				select {
				case events <- next:
				default:
					select {
					case <-events:
					default:
					}
					select {
					case events <- next:
					case <-done:
						return
					}
				}
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
		})
	}
	return initial, events, stop
}

// WatchSignals intercepts terminal-directed host signals while a child owns
// the terminal. The handoff forwards each signal to the child's process group.
func WatchSignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(signals)
		})
	}
	return signals, stop
}

func terminalFD(stream any) (uintptr, bool) {
	file, ok := stream.(interface{ Fd() uintptr })
	if !ok {
		return 0, false
	}
	return file.Fd(), true
}

func readWindowSize(fd uintptr) (WindowSize, bool) {
	width, height, err := xterm.GetSize(fd)
	if err != nil || width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return WindowSize{}, false
	}
	return WindowSize{Width: uint16(width), Height: uint16(height)}, true
}
