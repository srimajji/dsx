package terminal

import (
	"errors"
	"os"
	"sync"

	charmterm "github.com/charmbracelet/x/term"
)

// RawState transfers a physical terminal from the restored application state
// into raw mode for a PTY child, then restores the exact prior state.
type RawState struct {
	file *os.File

	mu       sync.Mutex
	previous *charmterm.State
	released bool
}

func NewRawState(file *os.File) *RawState {
	return &RawState{file: file}
}

func (state *RawState) Release() error {
	if state == nil || state.file == nil {
		return errors.New("physical terminal input is unavailable")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return errors.New("physical terminal is already in raw mode")
	}
	fd := state.file.Fd()
	if !charmterm.IsTerminal(fd) {
		return errors.New("physical terminal input is not a TTY")
	}
	previous, err := charmterm.MakeRaw(fd)
	if err != nil {
		return err
	}
	state.previous = previous
	state.released = true
	return nil
}

func (state *RawState) Restore() error {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.released {
		return nil
	}
	err := charmterm.Restore(state.file.Fd(), state.previous)
	if err == nil {
		state.previous = nil
		state.released = false
	}
	return err
}

var _ TerminalState = (*RawState)(nil)
