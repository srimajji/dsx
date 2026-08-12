package model

import "fmt"

type WorkspaceState string

const (
	StatePlanned         WorkspaceState = "planned"
	StateCreating        WorkspaceState = "creating"
	StateRunning         WorkspaceState = "running"
	StateStopped         WorkspaceState = "stopped"
	StateNeedsResolution WorkspaceState = "needs_resolution"
	StateFailed          WorkspaceState = "failed"
	StateCleaning        WorkspaceState = "cleaning"
	StateDeleted         WorkspaceState = "deleted"
)

var workspaceTransitions = map[WorkspaceState]map[WorkspaceState]struct{}{
	StatePlanned:         {StateCreating: {}, StateCleaning: {}},
	StateCreating:        {StateRunning: {}, StateFailed: {}, StateCleaning: {}},
	StateRunning:         {StateStopped: {}, StateNeedsResolution: {}, StateFailed: {}, StateCleaning: {}},
	StateStopped:         {StateRunning: {}, StateNeedsResolution: {}, StateFailed: {}, StateCleaning: {}},
	StateNeedsResolution: {StateStopped: {}, StateRunning: {}, StateFailed: {}, StateCleaning: {}},
	StateFailed:          {StateStopped: {}, StateCleaning: {}},
	StateCleaning:        {StateDeleted: {}, StateFailed: {}},
	StateDeleted:         {},
}

func (state WorkspaceState) Valid() bool {
	_, ok := workspaceTransitions[state]
	return ok
}

func (state WorkspaceState) CanTransitionTo(next WorkspaceState) bool {
	allowed, ok := workspaceTransitions[state]
	if !ok {
		return false
	}
	_, ok = allowed[next]
	return ok
}

func (state WorkspaceState) Transition(next WorkspaceState) error {
	if !state.Valid() || !next.Valid() || !state.CanTransitionTo(next) {
		return fmt.Errorf("invalid workspace transition %q -> %q", state, next)
	}
	return nil
}
