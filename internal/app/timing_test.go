package app

import (
	"context"
	"encoding/json"
	"strings"
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

func TestLifecyclePhaseTimingsAreOrderedDeterministicAndPayloadFree(t *testing.T) {
	service, root, _, _, approvalHash := lifecycleFixture(t)
	clock := &stepClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), step: time.Millisecond}
	var recorded timingCollector
	service.timingRecorder = &recorded
	service.timingClock = clock.Next
	service.inspection.timingRecorder = &recorded
	service.inspection.timingClock = clock.Next

	if _, err := service.Start(context.Background(), StartRequest{Root: root, ApproveConfig: approvalHash}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	const secret = "timing-must-not-record-this-secret"
	if _, err := service.Shell(context.Background(), ShellRequest{
		Root: root,
		Argv: []string{"/bin/true"},
		Env:  map[string]string{"PERFORMANCE_TEST_TOKEN": secret},
	}); err != nil {
		t.Fatalf("Shell() error = %v", err)
	}
	if _, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true}); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}

	want := []PhaseTiming{
		{Phase: PhasePlanning, ElapsedNS: time.Millisecond},
		{Phase: PhaseInspect, ElapsedNS: 3 * time.Millisecond},
		{Phase: PhasePlanning, ElapsedNS: time.Millisecond},
		{Phase: PhaseInspect, ElapsedNS: 3 * time.Millisecond},
		{Phase: PhasePlanning, ElapsedNS: time.Millisecond},
		{Phase: PhaseInspect, ElapsedNS: 3 * time.Millisecond},
		{Phase: PhaseStart, ElapsedNS: 13 * time.Millisecond},
		{Phase: PhasePlanning, ElapsedNS: time.Millisecond},
		{Phase: PhaseInspect, ElapsedNS: 3 * time.Millisecond},
		{Phase: PhaseShell, ElapsedNS: 5 * time.Millisecond},
		{Phase: PhaseClean, ElapsedNS: time.Millisecond},
	}
	if len(recorded) != len(want) {
		t.Fatalf("timing count = %d, want %d: %#v", len(recorded), len(want), recorded)
	}
	for index := range want {
		if recorded[index] != want[index] {
			t.Fatalf("timing[%d] = %#v, want %#v", index, recorded[index], want[index])
		}
	}

	encoded, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, root, approvalHash, "PERFORMANCE_TEST_TOKEN"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("timing evidence contains forbidden request payload %q: %s", forbidden, encoded)
		}
	}
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
