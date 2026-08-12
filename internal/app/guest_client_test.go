package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

const (
	guestTestServerA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	guestTestServerB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var guestTestUUIDs = []string{
	"00000000-0000-4000-8000-000000000001",
	"00000000-0000-4000-8000-000000000002",
	"00000000-0000-4000-8000-000000000003",
	"00000000-0000-4000-8000-000000000004",
	"00000000-0000-4000-8000-000000000005",
	"00000000-0000-4000-8000-000000000006",
	"00000000-0000-4000-8000-000000000007",
	"00000000-0000-4000-8000-000000000008",
	"00000000-0000-4000-8000-000000000009",
	"00000000-0000-4000-8000-00000000000a",
	"00000000-0000-4000-8000-00000000000b",
	"00000000-0000-4000-8000-00000000000c",
	"00000000-0000-4000-8000-00000000000d",
	"00000000-0000-4000-8000-00000000000e",
	"00000000-0000-4000-8000-00000000000f",
	"00000000-0000-4000-8000-000000000010",
	"00000000-0000-4000-8000-000000000011",
	"00000000-0000-4000-8000-000000000012",
	"00000000-0000-4000-8000-000000000013",
	"00000000-0000-4000-8000-000000000014",
}

type guestClientExecCall struct {
	workspace runtime.ResourceSnapshot
	spec      runtime.ExecSpec
	stdin     []byte
}

type guestClientAdapter struct {
	mu      sync.Mutex
	calls   []guestClientExecCall
	handler func(context.Context, guestClientExecCall, runtime.ExecIO) (runtime.Exit, error)
}

func (adapter *guestClientAdapter) Exec(ctx context.Context, workspace runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	input, err := io.ReadAll(streams.Stdin)
	if err != nil {
		return runtime.Exit{}, err
	}
	call := guestClientExecCall{
		workspace: workspace,
		spec: runtime.ExecSpec{
			Argv:       append([]string(nil), spec.Argv...),
			Env:        append([]string(nil), spec.Env...),
			WorkingDir: spec.WorkingDir,
			User:       spec.User,
			Terminal:   spec.Terminal,
		},
		stdin: append([]byte(nil), input...),
	}
	adapter.mu.Lock()
	adapter.calls = append(adapter.calls, call)
	adapter.mu.Unlock()
	return adapter.handler(ctx, call, streams)
}

func (adapter *guestClientAdapter) Probe(context.Context) (runtime.Capabilities, error) {
	return runtime.Capabilities{}, errors.New("unexpected Probe")
}
func (adapter *guestClientAdapter) EnsureImage(context.Context, runtime.ImageSpec) (runtime.Image, error) {
	return runtime.Image{}, errors.New("unexpected EnsureImage")
}
func (adapter *guestClientAdapter) CreateVolume(context.Context, runtime.VolumeSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateVolume")
}
func (adapter *guestClientAdapter) CreateAuthLoginVolume(context.Context, runtime.AuthLoginVolumeSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateAuthLoginVolume")
}
func (adapter *guestClientAdapter) CreateNetwork(context.Context, runtime.NetworkSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateNetwork")
}
func (adapter *guestClientAdapter) CreateWorkspace(context.Context, runtime.WorkspaceSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateWorkspace")
}
func (adapter *guestClientAdapter) CreateBrowser(context.Context, runtime.BrowserSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateBrowser")
}
func (adapter *guestClientAdapter) CreateAuthLogin(context.Context, runtime.AuthLoginSpec) (runtime.Resource, error) {
	return runtime.Resource{}, errors.New("unexpected CreateAuthLogin")
}
func (adapter *guestClientAdapter) StartWorkspace(context.Context, runtime.ResourceSnapshot) error {
	return errors.New("unexpected StartWorkspace")
}
func (adapter *guestClientAdapter) StartAuthLogin(context.Context, runtime.ResourceSnapshot) error {
	return errors.New("unexpected StartAuthLogin")
}
func (adapter *guestClientAdapter) PrepareExec(context.Context, runtime.ResourceSnapshot, runtime.ExecSpec) (runtime.ProcessSpec, error) {
	return runtime.ProcessSpec{}, errors.New("unexpected PrepareExec")
}
func (adapter *guestClientAdapter) CopyTo(context.Context, runtime.ResourceSnapshot, runtime.HostPath, runtime.GuestPath) error {
	return errors.New("unexpected CopyTo")
}
func (adapter *guestClientAdapter) CopyFrom(context.Context, runtime.ResourceSnapshot, runtime.GuestPath, runtime.HostPath) error {
	return errors.New("unexpected CopyFrom")
}
func (adapter *guestClientAdapter) Inspect(context.Context, runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	return runtime.ResourceSnapshot{}, errors.New("unexpected Inspect")
}
func (adapter *guestClientAdapter) List(context.Context, runtime.ResourceKind) ([]runtime.ResourceSnapshot, error) {
	return nil, errors.New("unexpected List")
}
func (adapter *guestClientAdapter) Signal(context.Context, runtime.ResourceSnapshot, runtime.Signal) error {
	return errors.New("unexpected Signal")
}
func (adapter *guestClientAdapter) Stop(context.Context, runtime.ResourceSnapshot, runtime.StopPolicy) error {
	return errors.New("unexpected Stop")
}
func (adapter *guestClientAdapter) Delete(context.Context, runtime.ResourceSnapshot) error {
	return errors.New("unexpected Delete")
}

func TestGuestClientStartUsesExactStructuredExecAndApprovedGraph(t *testing.T) {
	workspace := guestClientWorkspace()
	var startRequest guestproto.Request
	var startParams guestproto.StartParams
	adapter := &guestClientAdapter{}
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		switch request.Operation {
		case guestproto.OperationPing:
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, struct{}{}, nil)
		case guestproto.OperationStart:
			startRequest = request
			if err := guestproto.DecodeParams(request.Params, &startParams); err != nil {
				t.Fatal(err)
			}
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, guestproto.StartResult{Generation: 8}, nil)
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
			return runtime.Exit{}, nil
		}
	}
	client := guestClientForTest(adapter, 3, 8192)
	approved := plan.ExecutionPlan{
		Setup: []plan.ResolvedCommand{
			{Argv: []string{"npm", "ci", "--ignore-scripts"}, Cwd: "/workspace", Env: []plan.EnvGrant{{Name: "MODE", Value: "test"}}},
			{Shell: "printf '%s' ready", ShellPath: "/bin/bash", Cwd: "/workspace"},
		},
		Processes: []plan.ResolvedProcess{{
			Name: "web",
			Command: plan.ResolvedCommand{
				Shell: "exec ./server --port 3000", ShellPath: "/bin/sh", Cwd: "/workspace/web",
				Env: []plan.EnvGrant{{Name: "TOKEN", Value: "approved-secret", Reference: "secret://token", Secret: true}},
			},
			Required: true,
			Health: &plan.ResolvedHealth{
				Kind: "command", IntervalMS: 100, TimeoutMS: 50, Retries: 3,
				Command: &plan.ResolvedCommand{Argv: []string{"healthcheck", "--quiet"}, Cwd: "/workspace/web"},
			},
		}},
	}
	result, err := client.Start(context.Background(), workspace, approved, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != 8 {
		t.Fatalf("Start generation = %d, want 8", result.Generation)
	}
	if startRequest.IfGeneration == nil || *startRequest.IfGeneration != 7 || startRequest.Target != "" {
		t.Fatalf("Start envelope generation/target = %#v/%q", startRequest.IfGeneration, startRequest.Target)
	}
	if startRequest.IdempotencyKey != guestTestUUIDs[0] {
		t.Fatalf("Start idempotency key = %q, want %q", startRequest.IdempotencyKey, guestTestUUIDs[0])
	}
	wantSetup := []guestproto.CommandSpec{
		{Argv: []string{"npm", "ci", "--ignore-scripts"}, Cwd: "/workspace", Env: []string{"MODE=test"}},
		{Argv: []string{"/bin/bash", "-lc", "printf '%s' ready"}, Cwd: "/workspace"},
	}
	if !reflect.DeepEqual(startParams.Setup, wantSetup) {
		t.Fatalf("Start setup = %#v, want %#v", startParams.Setup, wantSetup)
	}
	if got := startParams.Processes[0].Command; !reflect.DeepEqual(got.Argv, []string{"/bin/sh", "-lc", "exec ./server --port 3000"}) || !reflect.DeepEqual(got.Env, []string{"TOKEN=approved-secret"}) {
		t.Fatalf("Start process command = %#v", got)
	}
	if got := startParams.Processes[0].Health.Command.Argv; !reflect.DeepEqual(got, []string{"healthcheck", "--quiet"}) {
		t.Fatalf("health argv = %#v", got)
	}
	if startParams.LogLimitBytes != 8192 {
		t.Fatalf("log limit = %d, want 8192", startParams.LogLimitBytes)
	}
	if len(adapter.calls) != 2 {
		t.Fatalf("Exec calls = %d, want ping + start", len(adapter.calls))
	}
	for _, call := range adapter.calls {
		if !reflect.DeepEqual(call.workspace, workspace) {
			t.Fatalf("Exec workspace = %#v, want %#v", call.workspace, workspace)
		}
		if !reflect.DeepEqual(call.spec.Argv, []string{DefaultGuestHelperPath, "ctl", "--socket", DefaultGuestSocketPath}) {
			t.Fatalf("Exec argv = %#v", call.spec.Argv)
		}
		if len(call.spec.Env) != 0 || call.spec.WorkingDir != "" || call.spec.User != "1000:1000" || call.spec.Terminal {
			t.Fatalf("Exec authority widened: %#v", call.spec)
		}
		if len(call.stdin) == 0 || call.stdin[0] != '{' || call.stdin[len(call.stdin)-1] != '}' {
			t.Fatalf("Exec stdin is not one compact JSON request: %q", call.stdin)
		}
	}
}

func TestGuestClientDefaultsApprovedCommandWorkingDirectoryToWorkspace(t *testing.T) {
	client := guestClientForTest(&guestClientAdapter{}, 1, 4096)
	params, err := client.startParams(plan.ExecutionPlan{
		Setup: []plan.ResolvedCommand{{Argv: []string{"/bin/true"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(params.Setup) != 1 || params.Setup[0].Cwd != "/workspace" {
		t.Fatalf("setup = %#v", params.Setup)
	}
}

func TestGuestClientMutationRetryPreservesBytesAndKey(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{}
	var startInputs [][]byte
	startAttempts := 0
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		if request.Operation == guestproto.OperationPing {
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, struct{}{}, nil)
		}
		if request.Operation != guestproto.OperationStart {
			t.Fatalf("unexpected operation %q", request.Operation)
		}
		startInputs = append(startInputs, append([]byte(nil), call.stdin...))
		startAttempts++
		if startAttempts == 1 {
			return runtime.Exit{}, errors.New("transport failed with secret=do-not-leak")
		}
		return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, guestproto.StartResult{Generation: 1}, nil)
	}
	client := guestClientForTest(adapter, 3, 4096)
	result, err := client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"true"}, Cwd: "/"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != 1 || startAttempts != 2 {
		t.Fatalf("Start = %#v, attempts %d", result, startAttempts)
	}
	if !bytes.Equal(startInputs[0], startInputs[1]) {
		t.Fatalf("mutation retry changed bytes:\nfirst %q\nnext  %q", startInputs[0], startInputs[1])
	}
	first := decodeGuestClientRequest(t, startInputs[0])
	second := decodeGuestClientRequest(t, startInputs[1])
	if first.RequestID != second.RequestID || first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("retry identities changed: first %#v, second %#v", first, second)
	}
}

func TestGuestClientDoesNotRetryProtocolRejectionOrLeakSecrets(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{}
	startCalls := 0
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		if request.Operation == guestproto.OperationPing {
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, struct{}{}, nil)
		}
		startCalls++
		generation := uint64(41)
		protocolErr := &guestproto.Error{
			Code: guestproto.CodeGenerationConflict, Message: "TOKEN=super-secret", Generation: &generation,
			Details: json.RawMessage(`{"env":"TOKEN=super-secret"}`),
		}
		return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, nil, protocolErr)
	}
	client := guestClientForTest(adapter, 3, 4096)
	_, err := client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"true"}, Cwd: "/"}}}, 0)
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Start error = %v (code %q), want conflict", err, model.ErrorCodeOf(err))
	}
	if startCalls != 1 {
		t.Fatalf("protocol rejection start calls = %d, want 1", startCalls)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("protocol error leaked request/server data: %v", err)
	}
}

func TestGuestClientInstanceChangeStopsMutationReplay(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{}
	pingCalls := 0
	signalCalls := 0
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		switch request.Operation {
		case guestproto.OperationStatus:
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, emptyGuestStatus(), nil)
		case guestproto.OperationPing:
			pingCalls++
			server := guestTestServerA
			if pingCalls > 1 {
				server = guestTestServerB
			}
			return writeGuestClientResponse(t, streams.Stdout, request, server, struct{}{}, nil)
		case guestproto.OperationSignal:
			signalCalls++
			return runtime.Exit{}, errors.New("lost response")
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
			return runtime.Exit{}, nil
		}
	}
	client := guestClientForTest(adapter, 3, 4096)
	if _, err := client.Status(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	err := client.Signal(context.Background(), workspace, "web", 3, "SIGTERM")
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("Signal error = %v (code %q), want conflict", err, model.ErrorCodeOf(err))
	}
	if signalCalls != 1 || pingCalls != 2 {
		t.Fatalf("instance reconciliation calls: signal=%d ping=%d, want 1/2", signalCalls, pingCalls)
	}
}

func TestGuestClientReconcilesKnownWorkspaceRestartBeforeMutation(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{}
	server := guestTestServerA
	startCalls := 0
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		switch request.Operation {
		case guestproto.OperationStatus:
			return writeGuestClientResponse(t, streams.Stdout, request, server, emptyGuestStatus(), nil)
		case guestproto.OperationPing:
			return writeGuestClientResponse(t, streams.Stdout, request, server, struct{}{}, nil)
		case guestproto.OperationStart:
			startCalls++
			return writeGuestClientResponse(t, streams.Stdout, request, server, guestproto.StartResult{Generation: 1}, nil)
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
			return runtime.Exit{}, nil
		}
	}
	client := guestClientForTest(adapter, 2, 4096)
	if _, err := client.Status(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	server = guestTestServerB
	if err := client.Reconcile(context.Background(), workspace); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if _, err := client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"true"}, Cwd: "/"}}}, 0); err != nil {
		t.Fatalf("Start() after reconcile = %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
}

func TestGuestClientOperationsForwardTargetsGenerationsAndParams(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{}
	var operations []guestproto.Request
	adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		if request.Operation != guestproto.OperationPing {
			operations = append(operations, request)
		}
		var result any = struct{}{}
		switch request.Operation {
		case guestproto.OperationStatus:
			result = emptyGuestStatus()
		case guestproto.OperationWait:
			code := 23
			result = guestproto.ExitStatus{Code: &code}
		case guestproto.OperationPing, guestproto.OperationSignal, guestproto.OperationResize, guestproto.OperationShutdown:
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
		}
		return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, result, nil)
	}
	client := guestClientForTest(adapter, 2, 4096)
	if _, err := client.Status(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if err := client.Signal(context.Background(), workspace, "web", 9, "SIGINT"); err != nil {
		t.Fatal(err)
	}
	if err := client.Resize(context.Background(), workspace, "web", 10, 120, 40); err != nil {
		t.Fatal(err)
	}
	exit, err := client.Wait(context.Background(), workspace, "web", 11)
	if err != nil || exit.Code == nil || *exit.Code != 23 {
		t.Fatalf("Wait = %#v, %v", exit, err)
	}
	if err := client.Shutdown(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	wantOperations := []guestproto.Operation{guestproto.OperationStatus, guestproto.OperationSignal, guestproto.OperationResize, guestproto.OperationWait, guestproto.OperationShutdown}
	if len(operations) != len(wantOperations) {
		t.Fatalf("operations = %#v", operations)
	}
	for index, operation := range wantOperations {
		if operations[index].Operation != operation {
			t.Fatalf("operation[%d] = %q, want %q", index, operations[index].Operation, operation)
		}
	}
	for index, generation := range []uint64{9, 10, 11} {
		request := operations[index+1]
		if request.Target != "web" || request.IfGeneration == nil || *request.IfGeneration != generation {
			t.Fatalf("%s target/generation = %q/%v", request.Operation, request.Target, request.IfGeneration)
		}
	}
	var signal guestproto.SignalParams
	if err := guestproto.DecodeParams(operations[1].Params, &signal); err != nil || signal.Signal != "SIGINT" {
		t.Fatalf("signal params = %#v, %v", signal, err)
	}
	var resize guestproto.ResizeParams
	if err := guestproto.DecodeParams(operations[2].Params, &resize); err != nil || resize.Columns != 120 || resize.Rows != 40 {
		t.Fatalf("resize params = %#v, %v", resize, err)
	}
	if operations[4].IfGeneration != nil || operations[4].Target != "" || operations[4].IdempotencyKey == "" {
		t.Fatalf("shutdown envelope = %#v", operations[4])
	}
}

func TestGuestClientStrictResultAndRequestBounds(t *testing.T) {
	workspace := guestClientWorkspace()
	t.Run("oversized stdout", func(t *testing.T) {
		adapter := &guestClientAdapter{handler: func(_ context.Context, _ guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
			_, _ = streams.Stdout.Write(bytes.Repeat([]byte{'x'}, guestproto.MaxFrameSize+2))
			code := 0
			return runtime.Exit{Code: &code}, nil
		}}
		client := guestClientForTest(adapter, 1, 4096)
		_, err := client.Status(context.Background(), workspace)
		if model.ErrorCodeOf(err) != model.CodeUnavailable || len(adapter.calls) != 1 {
			t.Fatalf("oversized response error/calls = %v/%d", err, len(adapter.calls))
		}
	})
	t.Run("unknown result field", func(t *testing.T) {
		adapter := &guestClientAdapter{}
		adapter.handler = func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
			request := decodeGuestClientRequest(t, call.stdin)
			result := json.RawMessage(`{"generation":0,"failed":false,"processes":[],"unexpected":true}`)
			return writeGuestClientRawResponse(t, streams.Stdout, request, guestTestServerA, result)
		}
		client := guestClientForTest(adapter, 1, 4096)
		_, err := client.Status(context.Background(), workspace)
		if model.ErrorCodeOf(err) != model.CodeUnavailable {
			t.Fatalf("unknown result error = %v (code %q)", err, model.ErrorCodeOf(err))
		}
	})
	t.Run("usage exit is not retried", func(t *testing.T) {
		adapter := &guestClientAdapter{handler: func(_ context.Context, _ guestClientExecCall, _ runtime.ExecIO) (runtime.Exit, error) {
			code := 2
			return runtime.Exit{Code: &code}, nil
		}}
		client := guestClientForTest(adapter, 3, 4096)
		_, err := client.Status(context.Background(), workspace)
		if model.ErrorCodeOf(err) != model.CodeUnavailable || len(adapter.calls) != 1 {
			t.Fatalf("usage response error/calls = %v/%d", err, len(adapter.calls))
		}
	})
	t.Run("oversized request", func(t *testing.T) {
		adapter := &guestClientAdapter{handler: func(context.Context, guestClientExecCall, runtime.ExecIO) (runtime.Exit, error) {
			t.Fatal("oversized request reached Adapter.Exec")
			return runtime.Exit{}, nil
		}}
		environment := make([]plan.EnvGrant, guestproto.MaxEnvironment)
		for index := range environment {
			name := "E" + strings.Repeat("X", 3) + string(rune('a'+index%26)) + string(rune('a'+index/26))
			environment[index] = plan.EnvGrant{Name: name, Value: strings.Repeat("s", guestproto.MaxStringBytes-len(name)-1)}
		}
		client := guestClientForTest(adapter, 1, 4096)
		_, err := client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"true"}, Cwd: "/", Env: environment}}}, 0)
		if model.ErrorCodeOf(err) != model.CodeInvalidInput || len(adapter.calls) != 0 {
			t.Fatalf("oversized request error/calls = %v/%d", err, len(adapter.calls))
		}
	})
}

func TestGuestClientInvalidInputAndTransportErrorsDoNotLeakSecrets(t *testing.T) {
	workspace := guestClientWorkspace()
	adapter := &guestClientAdapter{handler: func(_ context.Context, call guestClientExecCall, streams runtime.ExecIO) (runtime.Exit, error) {
		request := decodeGuestClientRequest(t, call.stdin)
		if request.Operation == guestproto.OperationPing {
			return writeGuestClientResponse(t, streams.Stdout, request, guestTestServerA, struct{}{}, nil)
		}
		_, _ = streams.Stderr.Write([]byte("TOKEN=transport-secret"))
		return runtime.Exit{}, errors.New("TOKEN=adapter-secret")
	}}
	client := guestClientForTest(adapter, 1, 4096)
	_, err := client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"use", "approved-secret"}, Cwd: "/"}}}, 0)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("transport error leaked data: %v", err)
	}
	adapter.calls = nil
	_, err = client.Start(context.Background(), workspace, plan.ExecutionPlan{Setup: []plan.ResolvedCommand{{Argv: []string{"true"}, Cwd: "/", Env: []plan.EnvGrant{{Name: "TOKEN", Value: "bad\x00secret"}}}}}, 0)
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || strings.Contains(err.Error(), "secret") || len(adapter.calls) != 0 {
		t.Fatalf("validation error/calls = %v/%d", err, len(adapter.calls))
	}
	if err := client.Signal(context.Background(), workspace, "web", 0, "SIGSTOP"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("disallowed signal error = %v", err)
	}
}

func TestGuestClientDeadlineAndCancellation(t *testing.T) {
	workspace := guestClientWorkspace()
	t.Run("already canceled", func(t *testing.T) {
		adapter := &guestClientAdapter{handler: func(context.Context, guestClientExecCall, runtime.ExecIO) (runtime.Exit, error) {
			t.Fatal("canceled request reached Adapter.Exec")
			return runtime.Exit{}, nil
		}}
		client := guestClientForTest(adapter, 3, 4096)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Status(ctx, workspace)
		if !errors.Is(err, context.Canceled) || model.ErrorCodeOf(err) != model.CodeUnavailable || len(adapter.calls) != 0 {
			t.Fatalf("canceled Status error/calls = %v/%d", err, len(adapter.calls))
		}
	})
	t.Run("bounded deadline", func(t *testing.T) {
		adapter := &guestClientAdapter{handler: func(ctx context.Context, call guestClientExecCall, _ runtime.ExecIO) (runtime.Exit, error) {
			request := decodeGuestClientRequest(t, call.stdin)
			if request.DeadlineMS == 0 || request.DeadlineMS > 25 {
				t.Errorf("deadline_ms = %d, want 1..25", request.DeadlineMS)
			}
			<-ctx.Done()
			return runtime.Exit{}, ctx.Err()
		}}
		client := NewGuestClientWithDependencies(GuestClientDependencies{
			Adapter: adapter, RequestTimeout: 20 * time.Millisecond, MaxAttempts: 3,
			UUID: guestClientUUIDSource(t), LogLimitBytes: 4096, User: "1000:1000",
		})
		started := time.Now()
		_, err := client.Status(context.Background(), workspace)
		if !errors.Is(err, context.DeadlineExceeded) || model.ErrorCodeOf(err) != model.CodeUnavailable {
			t.Fatalf("deadline Status error = %v (code %q)", err, model.ErrorCodeOf(err))
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("deadline returned after %v", elapsed)
		}
		if len(adapter.calls) != 1 {
			t.Fatalf("deadline Exec calls = %d, want 1", len(adapter.calls))
		}
	})
}

func guestClientForTest(adapter runtime.Adapter, maxAttempts, logLimit int) *GuestClient {
	return NewGuestClientWithDependencies(GuestClientDependencies{
		Adapter: adapter, MaxAttempts: maxAttempts, LogLimitBytes: logLimit,
		UUID: guestClientUUIDSource(nil), User: "1000:1000",
	})
}

func guestClientUUIDSource(t *testing.T) func() (string, error) {
	index := 0
	return func() (string, error) {
		if index >= len(guestTestUUIDs) {
			if t != nil {
				t.Fatal("guest test UUID source exhausted")
			}
			return "", errors.New("UUID source exhausted")
		}
		value := guestTestUUIDs[index]
		index++
		return value, nil
	}
}

func guestClientWorkspace() runtime.ResourceSnapshot {
	return runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "workspace-id", Name: "workspace", Kind: runtime.ResourceWorkspace}, State: "running"}
}

func emptyGuestStatus() guestproto.StatusResult {
	return guestproto.StatusResult{Processes: make([]guestproto.ProcessStatus, 0)}
}

func decodeGuestClientRequest(t *testing.T, encoded []byte) guestproto.Request {
	t.Helper()
	request, err := guestproto.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRequest(%q): %v", encoded, err)
	}
	return request
}

func writeGuestClientResponse(t *testing.T, output io.Writer, request guestproto.Request, server string, result any, protocolErr *guestproto.Error) (runtime.Exit, error) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	response := guestproto.Response{
		Protocol: guestproto.ProtocolV1, RequestID: request.RequestID,
		OK: protocolErr == nil, Result: raw, Error: protocolErr,
		Server: guestproto.Server{InstanceID: server, Version: "test-v1"},
	}
	encoded, err := guestproto.EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if _, err := output.Write(encoded); err != nil {
		t.Fatal(err)
	}
	code := 0
	if protocolErr != nil {
		code = 1
	}
	return runtime.Exit{Code: &code}, nil
}

func writeGuestClientRawResponse(t *testing.T, output io.Writer, request guestproto.Request, server string, result json.RawMessage) (runtime.Exit, error) {
	t.Helper()
	response := guestproto.Response{
		Protocol: guestproto.ProtocolV1, RequestID: request.RequestID, OK: true, Result: result,
		Server: guestproto.Server{InstanceID: server, Version: "test-v1"},
	}
	encoded, err := guestproto.EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write(encoded); err != nil {
		t.Fatal(err)
	}
	code := 0
	return runtime.Exit{Code: &code}, nil
}
