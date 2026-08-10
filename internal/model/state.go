package model

import "fmt"

type WorkspaceMode string

const (
	ModeLive  WorkspaceMode = "live"
	ModeClone WorkspaceMode = "clone"
)

func ParseWorkspaceMode(value string) (WorkspaceMode, error) {
	switch WorkspaceMode(value) {
	case ModeLive, ModeClone:
		return WorkspaceMode(value), nil
	default:
		return "", fmt.Errorf("invalid workspace mode %q", value)
	}
}

type SandboxState string

const (
	StatePlanned  SandboxState = "planned"
	StateCreating SandboxState = "creating"
	StateRunning  SandboxState = "running"
	StateStopped  SandboxState = "stopped"
	StateFailed   SandboxState = "failed"
	StateCleaning SandboxState = "cleaning"
	StateDeleted  SandboxState = "deleted"
)

var sandboxTransitions = map[SandboxState]map[SandboxState]struct{}{
	StatePlanned:  {StateCreating: {}, StateCleaning: {}},
	StateCreating: {StateRunning: {}, StateFailed: {}, StateCleaning: {}},
	StateRunning:  {StateStopped: {}, StateFailed: {}, StateCleaning: {}},
	StateStopped:  {StateRunning: {}, StateCleaning: {}},
	StateFailed:   {StateCleaning: {}},
	StateCleaning: {StateDeleted: {}, StateFailed: {}},
	StateDeleted:  {},
}

func (state SandboxState) Valid() bool {
	_, ok := sandboxTransitions[state]
	return ok
}

func (state SandboxState) CanTransitionTo(next SandboxState) bool {
	allowed, ok := sandboxTransitions[state]
	if !ok {
		return false
	}
	_, ok = allowed[next]
	return ok
}

func (state SandboxState) Transition(next SandboxState) error {
	if !state.Valid() || !next.Valid() || !state.CanTransitionTo(next) {
		return fmt.Errorf("invalid sandbox transition %q -> %q", state, next)
	}
	return nil
}
