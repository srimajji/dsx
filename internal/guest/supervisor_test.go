package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/srimajji/dsx/internal/guestproto"
)

const (
	testInstanceID = "10000000-0000-4000-8000-000000000001"
	testRequestID  = "20000000-0000-4000-8000-000000000002"
	testKey        = "30000000-0000-4000-8000-000000000003"
)

func TestGuestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_DSX_GUEST_HELPER") != "1" {
		return
	}
	arguments := helperArguments(os.Args)
	if len(arguments) == 0 {
		os.Exit(90)
	}
	switch arguments[0] {
	case "append":
		appendFile(arguments[1], arguments[2]+"\n")
	case "service":
		appendFile(arguments[1], "service-start\n")
		delay, _ := strconv.Atoi(arguments[3])
		time.Sleep(time.Duration(delay) * time.Millisecond)
		appendFile(arguments[1], "service-ready\n")
		if err := os.WriteFile(arguments[2], []byte("ready"), 0o600); err != nil {
			os.Exit(91)
		}
		time.Sleep(30 * time.Second)
	case "dependent":
		appendFile(arguments[1], "dependent-start\n")
		time.Sleep(30 * time.Second)
	case "exists":
		if _, err := os.Stat(arguments[1]); err != nil {
			os.Exit(1)
		}
	case "exit":
		code, _ := strconv.Atoi(arguments[1])
		os.Exit(code)
	case "delay-exit":
		delay, _ := strconv.Atoi(arguments[1])
		code, _ := strconv.Atoi(arguments[2])
		time.Sleep(time.Duration(delay) * time.Millisecond)
		os.Exit(code)
	case "count-exit":
		appendFile(arguments[1], "run\n")
		code, _ := strconv.Atoi(arguments[2])
		os.Exit(code)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "group-parent":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		child := exec.Command(os.Args[0], "-test.run=^TestGuestHelperProcess$", "--", "group-child", arguments[1], arguments[3])
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(95)
		}
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(arguments[3]); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(96)
			}
			time.Sleep(time.Millisecond)
		}
		appendFile(arguments[2], strconv.Itoa(child.Process.Pid)+"\n")
		<-signals
		signal.Stop(signals)
		_ = child.Wait()
	case "group-child":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := os.WriteFile(arguments[2], []byte("ready"), 0o600); err != nil {
			os.Exit(97)
		}
		<-signals
		appendFile(arguments[1], "child-term\n")
		signal.Stop(signals)
	case "setsid-parent":
		child := exec.Command(os.Args[0], "-test.run=^TestGuestHelperProcess$", "--", "orphan-child", arguments[1])
		child.Env = os.Environ()
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(98)
		}
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(arguments[1]); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(99)
			}
			time.Sleep(time.Millisecond)
		}
	case "orphan-child":
		if err := os.WriteFile(arguments[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(100)
		}
		time.Sleep(30 * time.Second)
	case "flood":
		for index := 0; index < 300; index++ {
			fmt.Fprintf(os.Stdout, "stdout-%03d-xxxxxxxxxxxxxxxx\n", index)
			fmt.Fprintf(os.Stderr, "stderr-%03d-yyyyyyyyyyyyyyyy\n", index)
		}
	case "cwd-env":
		cwd, _ := os.Getwd()
		appendFile(arguments[1], cwd+"|"+os.Getenv("DIRECT_VALUE")+"\n")
	default:
		os.Exit(92)
	case "terminal-size":
		size, err := pty.GetsizeFull(os.Stdin)
		if err != nil {
			os.Exit(13)
		}
		initialRows, initialColumns := size.Rows, size.Cols
		_ = os.WriteFile(arguments[1], []byte(fmt.Sprintf("%dx%d", initialColumns, initialRows)), 0o600)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			size, err = pty.GetsizeFull(os.Stdin)
			if err == nil && (size.Rows != initialRows || size.Cols != initialColumns) {
				_ = os.WriteFile(arguments[1], []byte(fmt.Sprintf("%dx%d", size.Cols, size.Rows)), 0o600)
				fmt.Fprintln(os.Stdout, "pty-stdout")
				fmt.Fprintln(os.Stderr, "pty-stderr")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(14)
	}
	os.Exit(0)
}

func TestSupervisorRunsSetupAndDAGAfterReadiness(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	events := filepath.Join(directory, "events")
	ready := filepath.Join(directory, "ready")
	params := guestproto.StartParams{
		Setup: []guestproto.CommandSpec{helperCommand(t, directory, "append", events, "setup")},
		Processes: []guestproto.ProcessSpec{
			{ID: "service", Command: helperCommand(t, directory, "service", events, ready, "75"), Required: true, Health: &guestproto.HealthSpec{Kind: "command", Command: &guestproto.CommandSpec{Argv: []string{"/bin/test", "-e", ready}, Cwd: "/"}, IntervalMS: 20, TimeoutMS: 500, Retries: 100}},
			{ID: "dependent", Command: helperCommand(t, directory, "dependent", events), DependsOn: []string{"service"}, Required: true},
		},
		LogLimitBytes: 1024,
	}
	generation, err := supervisor.Start(context.Background(), 0, params)
	if err != nil || generation != 1 {
		t.Fatalf("Start() = (%d, %v)", generation, err)
	}
	waitStatus(t, supervisor, "dependent", func(status guestproto.ProcessStatus) bool { return status.Ready })
	var contents []byte
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		contents, _ = os.ReadFile(events)
		if strings.Contains(string(contents), "dependent-start\n") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := string(contents), "setup\nservice-start\nservice-ready\ndependent-start\n"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorPublishesUnhealthyAndRecoveredLiveness(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	healthy := filepath.Join(directory, "healthy")
	if err := os.WriteFile(healthy, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := guestproto.StartParams{
		Processes: []guestproto.ProcessSpec{{
			ID:       "service",
			Command:  helperCommand(t, directory, "sleep"),
			Required: true,
			Health: &guestproto.HealthSpec{
				Kind: "command", Command: &guestproto.CommandSpec{Argv: []string{"/bin/test", "-e", healthy}, Cwd: "/"},
				IntervalMS: 20, TimeoutMS: 500, Retries: 2,
			},
		}},
		LogLimitBytes: 128,
	}
	if _, err := supervisor.Start(context.Background(), 0, params); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, supervisor, "service", func(status guestproto.ProcessStatus) bool { return status.Ready })
	if err := os.Remove(healthy); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, supervisor, "service", func(status guestproto.ProcessStatus) bool {
		return status.State == guestproto.StateUnhealthy && !status.Ready && supervisor.Status().Failed
	})
	if err := os.WriteFile(healthy, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, supervisor, "service", func(status guestproto.ProcessStatus) bool {
		return status.State == guestproto.StateReady && status.Ready && !supervisor.Status().Failed
	})
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorRequiresAndAppliesNonRootChildIdentity(t *testing.T) {
	if _, err := NewSupervisor(Options{Version: "test", InstanceID: testInstanceID, ChildUID: 0, ChildGID: uint32(os.Getegid())}); err == nil {
		t.Fatal("NewSupervisor() accepted root child UID")
	}
	uid := uint32(os.Geteuid())
	if uid == 0 {
		t.Skip("host is root; runtime credential execution is covered by Linux integration")
	}
	gid := uint32(os.Getegid())
	command := commandFor(guestproto.CommandSpec{Argv: []string{"/usr/bin/true"}, Cwd: "/"}, nil, uid, gid)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid || command.SysProcAttr.Credential == nil {
		t.Fatalf("process attributes = %+v", command.SysProcAttr)
	}
	credential := command.SysProcAttr.Credential
	if credential.Uid != uid || credential.Gid != gid || !credential.NoSetGroups {
		t.Fatalf("child credential = %+v, want uid=%d gid=%d no-set-groups", credential, uid, gid)
	}
}

func TestSupervisorUsesDirectArgvCwdAndEnv(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	result := filepath.Join(directory, "result")
	command := helperCommand(t, directory, "cwd-env", result)
	command.Env = append(command.Env, "DIRECT_VALUE=argv-not-shell")
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "direct", Command: command}}, LogLimitBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	exit, err := supervisor.Wait(context.Background(), "direct", nil)
	if err != nil || exit.Code == nil || *exit.Code != 0 {
		t.Fatalf("Wait() = (%+v, %v)", exit, err)
	}
	contents, _ := os.ReadFile(result)
	canonicalDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), canonicalDirectory+"|argv-not-shell\n"; got != want {
		t.Fatalf("direct execution = %q, want %q", got, want)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorOwnsOneWaitAndSharesCanonicalExit(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "waited", Command: helperCommand(t, directory, "delay-exit", "75", "6")}}, LogLimitBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	type waitResult struct {
		exit *guestproto.ExitStatus
		err  error
	}
	results := make(chan waitResult, 8)
	for index := 0; index < cap(results); index++ {
		go func() {
			exit, waitErr := supervisor.Wait(context.Background(), "waited", nil)
			results <- waitResult{exit: exit, err: waitErr}
		}()
	}
	for index := 0; index < cap(results); index++ {
		result := <-results
		if result.err != nil || result.exit == nil || result.exit.Code == nil || *result.exit.Code != 6 {
			t.Fatalf("shared Wait() = (%+v, %v)", result.exit, result.err)
		}
	}
	shutdownSupervisor(t, supervisor)
}
func TestSupervisorResizesPTYAndMergesOutput(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	sizePath := filepath.Join(directory, "size")
	params := guestproto.StartParams{
		Processes: []guestproto.ProcessSpec{{
			ID: "terminal", Terminal: true,
			Command: helperCommand(t, directory, "terminal-size", sizePath),
		}},
		LogLimitBytes: 1024,
	}
	generation, err := supervisor.Start(context.Background(), 0, params)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if initial, readErr := os.ReadFile(sizePath); readErr == nil && len(initial) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY child did not observe its initial terminal size")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitStatus(t, supervisor, "terminal", func(status guestproto.ProcessStatus) bool { return status.Ready })
	if err := supervisor.Resize("terminal", generation, 120, 43); err != nil {
		t.Fatalf("Resize() = %v", err)
	}
	exit, err := supervisor.Wait(context.Background(), "terminal", &generation)
	if err != nil || exit == nil || exit.Code == nil || *exit.Code != 0 {
		t.Fatalf("Wait() = (%+v, %v)", exit, err)
	}
	size, err := os.ReadFile(sizePath)
	if err != nil || string(size) != "120x43" {
		t.Fatalf("resized terminal = %q, %v", size, err)
	}
	status := supervisor.Status().Processes[0]
	if !strings.Contains(status.Log, "pty-stdout") || !strings.Contains(status.Log, "pty-stderr") {
		t.Fatalf("merged PTY log = %q", status.Log)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorRequiredAndOptionalFailuresAndNoRestart(t *testing.T) {
	t.Run("optional", func(t *testing.T) {
		supervisor := newTestSupervisor(t, nil)
		count := filepath.Join(t.TempDir(), "count")
		_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "optional", Command: helperCommand(t, filepath.Dir(count), "count-exit", count, "4"), Required: false}}, LogLimitBytes: 128})
		if err != nil {
			t.Fatal(err)
		}
		exit, err := supervisor.Wait(context.Background(), "optional", nil)
		if err != nil || exit.Code == nil || *exit.Code != 4 || supervisor.Status().Failed {
			t.Fatalf("optional failure = exit %+v, err %v, status %+v", exit, err, supervisor.Status())
		}
		time.Sleep(75 * time.Millisecond)
		contents, _ := os.ReadFile(count)
		if strings.Count(string(contents), "run\n") != 1 {
			t.Fatalf("process restarted: %q", contents)
		}
		shutdownSupervisor(t, supervisor)
	})
	t.Run("required and shutdown", func(t *testing.T) {
		supervisor := newTestSupervisor(t, nil)
		directory := t.TempDir()
		_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "required", Command: helperCommand(t, directory, "exit", "7"), Required: true}}, LogLimitBytes: 128})
		if err != nil {
			t.Fatal(err)
		}
		exit, err := supervisor.Wait(context.Background(), "required", nil)
		if err != nil || exit.Code == nil || *exit.Code != 7 || !supervisor.Status().Failed {
			t.Fatalf("required failure = exit %+v, err %v, status %+v", exit, err, supervisor.Status())
		}
		shutdownSupervisor(t, supervisor)
		if err := supervisor.Shutdown(context.Background()); err != nil {
			t.Fatalf("idempotent Shutdown() = %v", err)
		}
	})
}

func TestSupervisorSignalUsesProcessGroupAndReportsSignal(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "sleeper", Command: helperCommand(t, directory, "sleep")}}, LogLimitBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, supervisor, "sleeper", func(status guestproto.ProcessStatus) bool { return status.Ready })
	supervisor.mu.Lock()
	pid := supervisor.processes["sleeper"].cmd.Process.Pid
	supervisor.mu.Unlock()
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid {
		t.Fatalf("process group = %d, pid %d, err %v", group, pid, err)
	}
	if err := supervisor.Signal("sleeper", 1, "KILL"); err != nil {
		t.Fatal(err)
	}
	exit, err := supervisor.Wait(context.Background(), "sleeper", nil)
	if err != nil || exit.Signal != "KILL" || exit.Code != nil {
		t.Fatalf("signal exit = (%+v, %v)", exit, err)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorSignalReachesWholeProcessGroup(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	childSignal := filepath.Join(directory, "child-signal")
	childPID := filepath.Join(directory, "child-pid")
	childReady := filepath.Join(directory, "child-ready")
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "grouped", Command: helperCommand(t, directory, "group-parent", childSignal, childPID, childReady)}}, LogLimitBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(childPID); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(childPID); err != nil {
		t.Fatalf("group child did not become ready: %v", err)
	}
	if err := supervisor.Signal("grouped", 1, "TERM"); err != nil {
		t.Fatal(err)
	}
	exit, err := supervisor.Wait(context.Background(), "grouped", nil)
	if err != nil || exit.Code == nil || *exit.Code != 0 {
		t.Fatalf("group parent exit = (%+v, %v)", exit, err)
	}
	contents, err := os.ReadFile(childSignal)
	if err != nil || string(contents) != "child-term\n" {
		t.Fatalf("child group signal = %q, %v", contents, err)
	}
	shutdownSupervisor(t, supervisor)
}
func TestSupervisorHealthTimeoutFailsRequiredGraph(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing")
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "unhealthy", Command: helperCommand(t, directory, "sleep"), Required: true, Health: &guestproto.HealthSpec{Kind: "command", Command: helperCommandPointer(t, directory, "exists", missing), IntervalMS: 5, TimeoutMS: 50, Retries: 2}}}, LogLimitBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	exit, err := supervisor.Wait(context.Background(), "unhealthy", nil)
	if err != nil || exit.Signal != "KILL" || !supervisor.Status().Failed {
		t.Fatalf("health timeout = exit %+v, err %v, status %+v", exit, err, supervisor.Status())
	}
	shutdownSupervisor(t, supervisor)
}

func TestShutdownCancelsAndReapsActiveHealthCommand(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	events := filepath.Join(directory, "health-events")
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{
		ID:      "probing",
		Command: helperCommand(t, directory, "sleep"),
		Health:  &guestproto.HealthSpec{Kind: "command", Command: helperCommandPointer(t, directory, "dependent", events), IntervalMS: 10, TimeoutMS: 60_000, Retries: 1},
	}}, LogLimitBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		contents, _ := os.ReadFile(events)
		if strings.Contains(string(contents), "dependent-start\n") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() with active health command = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Shutdown() took %v", elapsed)
	}
}

func TestSupervisorBoundsAndPrefixesMergedLogs(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	command := helperCommand(t, directory, "flood")
	command.Env = append(command.Env, "SECRET_VALUE=must-not-render")
	_, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "logger", Command: command}}, LogLimitBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Wait(context.Background(), "logger", nil); err != nil {
		t.Fatal(err)
	}
	status := supervisor.Status().Processes[0]
	if len(status.Log) > 256 || status.LogDropped == 0 {
		t.Fatalf("bounded log bytes=%d dropped=%d", len(status.Log), status.LogDropped)
	}
	for _, line := range strings.Split(strings.TrimSpace(status.Log), "\n") {
		if !strings.HasPrefix(line, "[logger] ") {
			t.Fatalf("unprefixed log line %q in %q", line, status.Log)
		}
	}
	if strings.Contains(status.Log, "must-not-render") {
		t.Fatal("environment secret appeared in logs")
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorGenerationAndIdempotency(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	params := guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "worker", Command: helperCommand(t, directory, "sleep")}}, LogLimitBytes: 128}
	raw, _ := json.Marshal(params)
	generation := uint64(0)
	request := guestproto.Request{Protocol: guestproto.ProtocolV1, RequestID: testRequestID, Operation: guestproto.OperationStart, IfGeneration: &generation, IdempotencyKey: testKey, DeadlineMS: 1000, Params: raw}
	first := supervisor.Handle(context.Background(), request)
	second := supervisor.Handle(context.Background(), request)
	if !first.OK || !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent replay mismatch: first=%+v second=%+v", first, second)
	}
	changed := request
	changed.RequestID = "20000000-0000-4000-8000-000000000004"
	changed.Params = append([]byte(nil), raw...)
	changed.Params = []byte(strings.Replace(string(changed.Params), `"log_limit_bytes":128`, `"log_limit_bytes":129`, 1))
	conflict := supervisor.Handle(context.Background(), changed)
	if conflict.OK || conflict.Error == nil || conflict.Error.Code != guestproto.CodeIdempotencyConflict {
		t.Fatalf("idempotency conflict = %+v", conflict)
	}
	wrong := request
	wrong.RequestID = "20000000-0000-4000-8000-000000000005"
	wrong.IdempotencyKey = "30000000-0000-4000-8000-000000000006"
	generation = 0
	wrong.IfGeneration = &generation
	generationConflict := supervisor.Handle(context.Background(), wrong)
	if generationConflict.OK || generationConflict.Error == nil || generationConflict.Error.Code != guestproto.CodeGenerationConflict {
		t.Fatalf("generation conflict = %+v", generationConflict)
	}
	shutdownSupervisor(t, supervisor)
}

func TestRunOneKillsSetsidDescendantAfterSuccessfulParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux subreaper behavior")
	}
	supervisor := newTestSupervisor(t, nil)
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	if err := runOne(context.Background(), helperCommand(t, t.TempDir(), "setsid-parent", pidFile), uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("setsid descendant %d survived successful parent: %v", pid, err)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorFastChildrenKeepCanonicalWaitStatusAndBoundAggregateLogs(t *testing.T) {
	supervisor := newTestSupervisor(t, nil)
	directory := t.TempDir()
	processes := make([]guestproto.ProcessSpec, 16)
	for index := range processes {
		processes[index] = guestproto.ProcessSpec{
			ID:      fmt.Sprintf("p%03d", index),
			Command: helperCommand(t, directory, "exit", "0"),
		}
	}
	if _, err := supervisor.Start(context.Background(), 0, guestproto.StartParams{Processes: processes, LogLimitBytes: guestproto.MaxLogBytes}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := supervisor.Status()
		complete := len(status.Processes) == len(processes)
		for _, process := range status.Processes {
			complete = complete && process.Exit != nil
			if process.Exit != nil && (process.Exit.Code == nil || *process.Exit.Code != 0 || process.Exit.Signal != "") {
				t.Fatalf("canonical exit lost for %s: %+v", process.ID, process.Exit)
			}
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fast children did not all exit: %+v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if retained := effectiveRetainedLogLimit(guestproto.MaxLogBytes, guestproto.MaxProcesses) * guestproto.MaxProcesses; retained > 32<<20 {
		t.Fatalf("aggregate retained log budget = %d", retained)
	}
	shutdownSupervisor(t, supervisor)
}

func newTestSupervisor(t *testing.T, output *strings.Builder) *Supervisor {
	t.Helper()
	var writer interface{ Write([]byte) (int, error) }
	if output != nil {
		writer = output
	}
	supervisor, err := NewSupervisor(Options{Version: "test", InstanceID: testInstanceID, ChildUID: uint32(os.Geteuid()), ChildGID: uint32(os.Getegid()), Output: writer, ShutdownGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

func helperCommand(t *testing.T, cwd string, arguments ...string) guestproto.CommandSpec {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{executable, "-test.run=^TestGuestHelperProcess$", "--"}
	argv = append(argv, arguments...)
	return guestproto.CommandSpec{Argv: argv, Cwd: cwd, Env: []string{"GO_WANT_DSX_GUEST_HELPER=1"}}
}

func helperCommandPointer(t *testing.T, cwd string, arguments ...string) *guestproto.CommandSpec {
	t.Helper()
	command := helperCommand(t, cwd, arguments...)
	return &command
}

func helperArguments(arguments []string) []string {
	for index, argument := range arguments {
		if argument == "--" && index+1 < len(arguments) {
			return arguments[index+1:]
		}
	}
	return nil
}

func appendFile(path, text string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(93)
	}
	_, err = file.WriteString(text)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		os.Exit(94)
	}
}

func waitStatus(t *testing.T, supervisor *Supervisor, id string, predicate func(guestproto.ProcessStatus) bool) guestproto.ProcessStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range supervisor.Status().Processes {
			if status.ID == id && predicate(status) {
				return status
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %+v", id, supervisor.Status())
	return guestproto.ProcessStatus{}
}

func shutdownSupervisor(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestChildEnvironmentPreservesSafeBaselineAndOverlaysDeclaredValues(t *testing.T) {
	t.Setenv("PATH", "/baseline/bin")
	t.Setenv("HOME", "/workspace/home")
	t.Setenv("HOST_SECRET", "must-not-cross")
	environment := childEnvironment([]string{"PATH=/project/bin", "MODE=test"})
	got := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("invalid environment entry %q", entry)
		}
		got[name] = value
	}
	if got["PATH"] != "/project/bin" || got["HOME"] != "/workspace/home" || got["MODE"] != "test" {
		t.Fatalf("child environment = %#v", got)
	}
	if _, leaked := got["HOST_SECRET"]; leaked {
		t.Fatalf("unapproved baseline variable crossed into child: %#v", got)
	}
}
