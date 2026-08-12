package apple_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/plan"
)

const (
	performanceRuns           = 5
	planningP95Budget         = 250 * time.Millisecond
	workspaceRestartP95Budget = 10 * time.Second
)

func TestPerformanceNamedWorkspacePlanningAndRestart(t *testing.T) {
	real := requireCoreRuntime(t)
	fixture := newWorkspaceFixture(t, real, real.adapter, "performance")
	defer fixture.recover()
	inspection := app.NewInspectionService(plan.NewResolver())

	planning := make([]time.Duration, 0, performanceRuns)
	for range performanceRuns {
		started := time.Now()
		if _, err := inspection.Inspect(context.Background(), app.InspectRequest{Root: fixture.root}); err != nil {
			t.Fatalf("warm inspect failed: %v", err)
		}
		planning = append(planning, time.Since(started))
	}
	assertPerformanceBudget(t, "planning", planning, planningP95Budget)

	fixture.create(context.Background())
	restarts := make([]time.Duration, 0, performanceRuns)
	for range performanceRuns {
		started := time.Now()
		if _, err := fixture.service.Restart(context.Background(), app.WorkspaceRestartRequest{Root: fixture.root, Workspace: fixture.workspace}); err != nil {
			t.Fatalf("workspace restart failed: %v", err)
		}
		restarts = append(restarts, time.Since(started))
	}
	assertPerformanceBudget(t, "workspace restart", restarts, workspaceRestartP95Budget)
	fixture.remove()
	fixture.assertRecovered()
}

func assertPerformanceBudget(t *testing.T, name string, samples []time.Duration, budget time.Duration) {
	t.Helper()
	p95 := nearestRankP95Duration(samples)
	if p95 > budget {
		t.Errorf("%s p95 = %s, budget %s; samples=%v", name, p95, budget, samples)
	}
}

func nearestRankP95Duration(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[(95*len(ordered)+99)/100-1]
}

func nearestRankP95Uint64(samples []uint64) uint64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]uint64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[(95*len(ordered)+99)/100-1]
}

func TestNearestRankP95Helpers(t *testing.T) {
	durations := []time.Duration{9, 1, 7, 3, 5, 2, 8, 4, 6, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := nearestRankP95Duration(durations); got != 19 {
		t.Fatalf("duration p95 = %s", got)
	}
	values := []uint64{9, 1, 7, 3, 5, 2, 8, 4, 6, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := nearestRankP95Uint64(values); got != 19 {
		t.Fatalf("uint64 p95 = %d", got)
	}
}
