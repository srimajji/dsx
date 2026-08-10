package apple_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	statefs "github.com/srimajji/dsx/internal/state/fs"
)

const (
	applePerformanceOptIn = "DSX_RUN_APPLE_PERFORMANCE"
	performanceEvidence   = "DSX_PERFORMANCE_EVIDENCE"
	performanceRuns       = 20
	guestRSSBudgetBytes   = uint64(100 * 1024 * 1024)
)

var performanceBudgets = map[string]time.Duration{
	"inspect":     500 * time.Millisecond,
	"planning":    250 * time.Millisecond,
	"shell_ready": 3 * time.Second,
	"clean":       5 * time.Second,
}

type performanceCollector struct {
	records []app.PhaseTiming
}

func (collector *performanceCollector) RecordPhaseTiming(timing app.PhaseTiming) {
	collector.records = append(collector.records, timing)
}

func (collector *performanceCollector) reset() {
	collector.records = collector.records[:0]
}

func (collector *performanceCollector) latest(t *testing.T, phase app.Phase) time.Duration {
	t.Helper()
	for index := len(collector.records) - 1; index >= 0; index-- {
		if collector.records[index].Phase == phase {
			return collector.records[index].ElapsedNS
		}
	}
	t.Fatalf("phase %q was not recorded", phase)
	return 0
}

type physicalPerformanceFixture struct {
	core       *coreFixture
	inspection *app.InspectionService
}

type performanceTimingEvidence struct {
	Name       string  `json:"name"`
	Phase      string  `json:"phase,omitempty"`
	BudgetNS   int64   `json:"budget_ns,omitempty"`
	SamplesNS  []int64 `json:"samples_ns"`
	P95NS      int64   `json:"p95_ns"`
	SampleSize int     `json:"sample_size"`
}

type performanceRSSEvidence struct {
	Process     string   `json:"process"`
	BudgetBytes uint64   `json:"budget_bytes"`
	Samples     []uint64 `json:"samples_bytes"`
	P95Bytes    uint64   `json:"p95_bytes"`
	MaxBytes    uint64   `json:"max_bytes"`
	SampleSize  int      `json:"sample_size"`
}

type physicalPerformanceEvidence struct {
	SchemaVersion int                         `json:"schema_version"`
	GeneratedAt   time.Time                   `json:"generated_at"`
	HostOS        string                      `json:"host_os"`
	HostArch      string                      `json:"host_arch"`
	CLIVersion    string                      `json:"container_cli_version"`
	ServerVersion string                      `json:"container_server_version"`
	Compatibility string                      `json:"compatibility_id"`
	Runs          int                         `json:"runs"`
	Timings       []performanceTimingEvidence `json:"timings"`
	GuestRSS      performanceRSSEvidence      `json:"guest_rss"`
}

func TestPerformance(t *testing.T) {
	if os.Getenv(applePerformanceOptIn) != "1" {
		t.Skipf("physical Apple performance suite disabled; set %s=1 on a dedicated Apple-silicon host", applePerformanceOptIn)
	}
	evidencePath := strings.TrimSpace(os.Getenv(performanceEvidence))
	if evidencePath == "" {
		t.Fatalf("%s must name the performance evidence JSON before the physical suite can run", performanceEvidence)
	}
	parent, err := os.Stat(filepath.Dir(evidencePath))
	if err != nil || !parent.IsDir() {
		t.Fatalf("performance evidence parent directory is unavailable: %v", err)
	}

	real := requireCoreRuntime(t)
	capabilities, err := real.adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() failed: %v", err)
	}
	collector := &performanceCollector{}
	fixture := newPhysicalPerformanceFixture(t, real, collector)
	defer fixture.core.recover()

	// One unmeasured lifecycle makes image, runtime, filesystem, and helper caches
	// warm before any acceptance sample is retained.
	performanceStart(t, fixture.core)
	performanceShell(t, fixture.core)
	fixture.core.clean()
	collector.reset()

	timingSamples := map[string][]time.Duration{
		"inspect":     make([]time.Duration, 0, performanceRuns),
		"planning":    make([]time.Duration, 0, performanceRuns),
		"start":       make([]time.Duration, 0, performanceRuns),
		"shell":       make([]time.Duration, 0, performanceRuns),
		"shell_ready": make([]time.Duration, 0, performanceRuns),
		"clean":       make([]time.Duration, 0, performanceRuns),
	}
	rssSamples := make([]uint64, 0, performanceRuns)

	for run := range performanceRuns {
		if _, err := fixture.inspection.Inspect(context.Background(), app.InspectRequest{Root: fixture.core.root, SandboxName: "main", Mode: string(model.ModeLive)}); err != nil {
			t.Fatalf("warm inspect run %d failed: %v", run+1, err)
		}
		timingSamples["planning"] = append(timingSamples["planning"], collector.latest(t, app.PhasePlanning))
		timingSamples["inspect"] = append(timingSamples["inspect"], collector.latest(t, app.PhaseInspect))

		performanceStart(t, fixture.core)
		timingSamples["start"] = append(timingSamples["start"], collector.latest(t, app.PhaseStart))
		rssSamples = append(rssSamples, readGuestRSS(t, fixture.core))

		shellStarted := time.Now()
		performanceShell(t, fixture.core)
		timingSamples["shell_ready"] = append(timingSamples["shell_ready"], time.Since(shellStarted))
		timingSamples["shell"] = append(timingSamples["shell"], collector.latest(t, app.PhaseShell))

		fixture.core.clean()
		timingSamples["clean"] = append(timingSamples["clean"], collector.latest(t, app.PhaseClean))
	}
	fixture.core.assertRecovered()

	evidence := physicalPerformanceEvidence{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC(),
		HostOS:        capabilities.HostOS,
		HostArch:      capabilities.HostArch,
		CLIVersion:    capabilities.CLIVersion,
		ServerVersion: capabilities.ServerVersion,
		Compatibility: capabilities.CompatibilityID,
		Runs:          performanceRuns,
		Timings: []performanceTimingEvidence{
			newTimingEvidence("inspect", app.PhaseInspect, performanceBudgets["inspect"], timingSamples["inspect"]),
			newTimingEvidence("planning", app.PhasePlanning, performanceBudgets["planning"], timingSamples["planning"]),
			newTimingEvidence("start", app.PhaseStart, 0, timingSamples["start"]),
			newTimingEvidence("shell", app.PhaseShell, 0, timingSamples["shell"]),
			newTimingEvidence("shell_ready", "", performanceBudgets["shell_ready"], timingSamples["shell_ready"]),
			newTimingEvidence("clean", app.PhaseClean, performanceBudgets["clean"], timingSamples["clean"]),
		},
		GuestRSS: performanceRSSEvidence{
			Process:     "dsx-guest",
			BudgetBytes: guestRSSBudgetBytes,
			Samples:     append([]uint64(nil), rssSamples...),
			P95Bytes:    nearestRankP95Uint64(rssSamples),
			MaxBytes:    maxUint64(rssSamples),
			SampleSize:  len(rssSamples),
		},
	}
	encoded := writePhysicalPerformanceEvidence(t, evidencePath, evidence)
	t.Logf("physical performance evidence:\n%s", encoded)

	for _, name := range []string{"inspect", "planning", "shell_ready", "clean"} {
		assertPerformanceBudget(t, name, timingSamples[name], performanceBudgets[name])
	}
	if evidence.GuestRSS.MaxBytes > guestRSSBudgetBytes {
		t.Errorf("dsx-guest RSS max = %d bytes, budget = %d bytes; samples = %v", evidence.GuestRSS.MaxBytes, guestRSSBudgetBytes, rssSamples)
	}
}

func newPhysicalPerformanceFixture(t *testing.T, real *coreRuntime, collector *performanceCollector) *physicalPerformanceFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := snapshotInventory(t, ctx, real)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeCoreConfig(t, root, false, 0)
	writeCoreGuestConfig(t, root)
	stateRoot := filepath.Join(t.TempDir(), "state")
	manifests, err := statefs.NewManifestRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := statefs.NewApprovalRepository(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	inspection := app.NewInspectionServiceWithDependencies(app.InspectionDependencies{Resolver: plan.NewResolver(), TimingRecorder: collector})
	inspected, err := inspection.Inspect(context.Background(), app.InspectRequest{Root: root, SandboxName: "main", Mode: string(model.ModeLive)})
	if err != nil {
		t.Fatalf("inspect performance fixture plan: %v", err)
	}
	helperPath, err := filepath.Abs(filepath.Join("..", "..", "bin", "dsx-guest"))
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.EvalSymlinks(helperPath)
	if err != nil {
		t.Fatalf("resolve built dsx-guest helper: %v", err)
	}
	helperCacheParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stagedHelper, err := app.StageGuestHelper(runtime.HostPath(helper), filepath.Join(helperCacheParent, "guest-helper"))
	if err != nil {
		t.Fatalf("stage built dsx-guest helper: %v", err)
	}
	service := app.NewLifecycleService(app.LifecycleDependencies{
		Inspection:     inspection,
		Approvals:      approvals,
		Manifests:      manifests,
		Locks:          manifests,
		Runtime:        real.adapter,
		Guest:          app.NewGuestClient(real.adapter),
		TimingRecorder: collector,
		GuestHelperSource: func() (runtime.HostPath, error) {
			return stagedHelper, nil
		},
	})
	core := &coreFixture{t: t, runtime: real, adapter: real.adapter, service: service, manifests: manifests, root: root, stateRoot: stateRoot, projectID: inspected.Plan.Project.ID, hash: inspected.Plan.ExecutableHash, before: before, guestHelper: stagedHelper}
	collector.reset()

	return &physicalPerformanceFixture{core: core, inspection: inspection}
}

func performanceStart(t *testing.T, fixture *coreFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture.start(ctx)
}

func performanceShell(t *testing.T, fixture *coreFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := fixture.service.Shell(ctx, app.ShellRequest{Root: fixture.root, Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("warm shell failed: %v", err)
	}
	if result.Exit.Code == nil || *result.Exit.Code != 0 {
		t.Fatalf("warm shell exit = %#v", result.Exit)
	}
}

func readGuestRSS(t *testing.T, fixture *coreFixture) uint64 {
	t.Helper()
	manifest := fixture.oneManifest()
	workspaceRecord := manifestRecord(t, manifest, string(runtime.ResourceWorkspace), "workspace")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace, err := fixture.adapter.Inspect(ctx, runtime.ResourceID(workspaceRecord.RuntimeID))
	if err != nil {
		t.Fatalf("inspect workspace for RSS: %v", err)
	}
	comm := readGuestProcFile(t, fixture.adapter, workspace, "/proc/1/comm")
	if strings.TrimSpace(comm) != "dsx-guest" {
		t.Fatalf("workspace PID 1 = %q, want dsx-guest", strings.TrimSpace(comm))
	}
	status := readGuestProcFile(t, fixture.adapter, workspace, "/proc/1/status")
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "VmRSS:" || fields[2] != "kB" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kilobytes > ^uint64(0)/1024 {
			t.Fatalf("parse dsx-guest VmRSS %q: %v", line, err)
		}
		return kilobytes * 1024
	}
	t.Fatalf("dsx-guest /proc/1/status omitted VmRSS: %s", status)
	return 0
}

func readGuestProcFile(t *testing.T, adapter runtime.Adapter, workspace runtime.ResourceSnapshot, path string) string {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	exit, err := adapter.Exec(ctx, workspace, runtime.ExecSpec{Argv: []string{"/bin/cat", path}, WorkingDir: "/", User: user}, runtime.ExecIO{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("read guest proc file %s: %v", path, err)
	}
	if exit.Code == nil || *exit.Code != 0 {
		t.Fatalf("read guest proc file %s exit = %#v, stderr = %q", path, exit, stderr.String())
	}
	return stdout.String()
}

func newTimingEvidence(name string, phase app.Phase, budget time.Duration, samples []time.Duration) performanceTimingEvidence {
	nanoseconds := make([]int64, len(samples))
	for index, sample := range samples {
		nanoseconds[index] = sample.Nanoseconds()
	}
	return performanceTimingEvidence{Name: name, Phase: string(phase), BudgetNS: budget.Nanoseconds(), SamplesNS: nanoseconds, P95NS: nearestRankP95Duration(samples).Nanoseconds(), SampleSize: len(samples)}
}

func assertPerformanceBudget(t *testing.T, name string, samples []time.Duration, budget time.Duration) {
	t.Helper()
	if len(samples) < performanceRuns {
		t.Errorf("%s sample count = %d, need at least %d", name, len(samples), performanceRuns)
		return
	}
	p95 := nearestRankP95Duration(samples)
	if p95 > budget {
		t.Errorf("%s p95 = %s, budget = %s; samples = %v", name, p95, budget, samples)
	}
}

func nearestRankP95Duration(samples []time.Duration) time.Duration {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	if len(ordered) == 0 {
		return 0
	}
	rank := (95*len(ordered) + 99) / 100
	return ordered[rank-1]
}

func nearestRankP95Uint64(samples []uint64) uint64 {
	ordered := append([]uint64(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	if len(ordered) == 0 {
		return 0
	}
	rank := (95*len(ordered) + 99) / 100
	return ordered[rank-1]
}

func maxUint64(samples []uint64) uint64 {
	var maximum uint64
	for _, sample := range samples {
		if sample > maximum {
			maximum = sample
		}
	}
	return maximum
}

func writePhysicalPerformanceEvidence(t *testing.T, path string, evidence physicalPerformanceEvidence) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatalf("encode physical performance evidence: %v", err)
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".dsx-performance-*.json")
	if err != nil {
		t.Fatalf("create physical performance evidence temporary: %v", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		t.Fatalf("secure physical performance evidence temporary: %v", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		t.Fatalf("write physical performance evidence: %v", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		t.Fatalf("sync physical performance evidence: %v", err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatalf("close physical performance evidence: %v", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		t.Fatalf("publish physical performance evidence: %v", err)
	}
	return encoded
}

func TestNearestRankP95Helpers(t *testing.T) {
	durations := make([]time.Duration, 20)
	integers := make([]uint64, 20)
	for index := range durations {
		// Reverse input proves helpers do not depend on arrival order.
		durations[index] = time.Duration(20-index) * time.Millisecond
		integers[index] = uint64(20 - index)
	}
	if got := nearestRankP95Duration(durations); got != 19*time.Millisecond {
		t.Fatalf("duration p95 = %s, want 19ms", got)
	}
	if got := nearestRankP95Uint64(integers); got != 19 {
		t.Fatalf("integer p95 = %d, want 19", got)
	}
	if durations[0] != 20*time.Millisecond || durations[19] != time.Millisecond || integers[0] != 20 || integers[19] != 1 {
		t.Fatal("percentile helpers mutated input samples")
	}
}
