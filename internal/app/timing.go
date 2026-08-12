package app

import "time"

// Phase identifies one bounded application operation. Values are deliberately
// low-cardinality and must not include project, workspace, run, or user input.
type Phase string

const (
	// PhaseInspect spans the complete read-only inspection, including planning.
	PhaseInspect Phase = "inspect"
	// PhasePlanning spans only deterministic plan resolution within inspection.
	PhasePlanning Phase = "planning"
	// PhaseStart spans the public lifecycle start operation.
	PhaseStart Phase = "start"
	// PhaseShell spans validation and readiness through child-process handoff;
	// interactive session lifetime is deliberately excluded.
	PhaseShell Phase = "shell"
	// PhaseClean spans the public ownership-safe cleanup operation.
	PhaseClean Phase = "clean"
)

// PhaseTiming is safe to retain as performance evidence: it contains only a
// fixed phase name and elapsed monotonic-clock duration.
type PhaseTiming struct {
	Phase     Phase         `json:"phase"`
	ElapsedNS time.Duration `json:"elapsed_ns"`
}

// PhaseTimingRecorder receives completed phase timings synchronously. Recorder
// implementations that are shared across goroutines are responsible for their
// own synchronization.
type PhaseTimingRecorder interface {
	RecordPhaseTiming(PhaseTiming)
}

// PhaseTimingRecorderFunc adapts a function to PhaseTimingRecorder.
type PhaseTimingRecorderFunc func(PhaseTiming)

func (record PhaseTimingRecorderFunc) RecordPhaseTiming(timing PhaseTiming) {
	record(timing)
}

type phaseTimer struct {
	recorder PhaseTimingRecorder
	clock    func() time.Time
	phase    Phase
	started  time.Time
	stopped  bool
}

func beginPhase(recorder PhaseTimingRecorder, clock func() time.Time, phase Phase) phaseTimer {
	if recorder == nil {
		return phaseTimer{}
	}
	if clock == nil {
		clock = time.Now
	}
	return phaseTimer{recorder: recorder, clock: clock, phase: phase, started: clock()}
}

func (timer *phaseTimer) Stop() {
	if timer == nil || timer.recorder == nil || timer.stopped {
		return
	}
	timer.stopped = true
	timer.recorder.RecordPhaseTiming(PhaseTiming{Phase: timer.phase, ElapsedNS: timer.clock().Sub(timer.started)})
}
