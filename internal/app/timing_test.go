package app

import (
	"testing"
	"time"
)

type timingCollector []PhaseTiming

func (collector *timingCollector) RecordPhaseTiming(timing PhaseTiming) {
	*collector = append(*collector, timing)
}

type stepClock struct {
	now  time.Time
	step time.Duration
}

func (clock *stepClock) Next() time.Time {
	current := clock.now
	clock.now = clock.now.Add(clock.step)
	return current
}

func TestDisabledPhaseTimingDoesNotReadClockOrAllocate(t *testing.T) {
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return time.Time{}
	}
	allocations := testing.AllocsPerRun(1000, func() {
		timing := beginPhase(nil, clock, PhaseInspect)
		timing.Stop()
	})
	if allocations != 0 {
		t.Fatalf("disabled phase timing allocations = %v, want 0", allocations)
	}
	if clockCalls != 0 {
		t.Fatalf("disabled phase timing read clock %d times", clockCalls)
	}
}
