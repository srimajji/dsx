package model

import "testing"

func TestStateTransitions(t *testing.T) {
	allowed := [][2]SandboxState{
		{StatePlanned, StateCreating},
		{StatePlanned, StateCleaning},
		{StateCreating, StateRunning},
		{StateRunning, StateStopped},
		{StateStopped, StateCleaning},
		{StateCleaning, StateDeleted},
	}
	for _, transition := range allowed {
		if err := transition[0].Transition(transition[1]); err != nil {
			t.Errorf("%s -> %s: %v", transition[0], transition[1], err)
		}
	}
	if err := StateDeleted.Transition(StateRunning); err == nil {
		t.Fatal("deleted sandbox returned to running")
	}
}

func TestErrorExitCodes(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("nil exit = %d", got)
	}
	if got := ExitCode(NewError(CodeUnapproved, "approval required", nil)); got != 3 {
		t.Fatalf("unapproved exit = %d", got)
	}
	if got := ExitCode(NewError(CodeUnavailable, "runtime missing", nil)); got != 4 {
		t.Fatalf("unavailable exit = %d", got)
	}
}
