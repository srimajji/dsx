package app

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestEncodeSecretEnvironmentIsDeterministicAndNULTerminated(t *testing.T) {
	got, err := encodeSecretEnvironment(map[string]string{"Z_TOKEN": "last", "A_HEADER": "first"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "A_HEADER=first\x00Z_TOKEN=last\x00"; string(got) != want {
		t.Fatalf("encodeSecretEnvironment() = %q, want %q", got, want)
	}
	ordinary, secret, err := partitionExecEnvironment(
		map[string]string{"VISIBLE": "ordinary", "A_HEADER": "first"},
		[]string{"A_HEADER"},
	)
	if err != nil || !reflect.DeepEqual(ordinary, map[string]string{"VISIBLE": "ordinary"}) || !reflect.DeepEqual(secret, map[string]string{"A_HEADER": "first"}) {
		t.Fatalf("partitionExecEnvironment() = (%#v, %#v, %v)", ordinary, secret, err)
	}
}

func TestSecretEnvironmentEncodingRejectsNULAndOversize(t *testing.T) {
	if _, _, err := partitionExecEnvironment(map[string]string{"TOKEN": "before\x00after"}, []string{"TOKEN"}); err == nil {
		t.Fatal("accepted NUL environment value")
	}
	if _, err := encodeSecretEnvironment(map[string]string{"TOKEN": strings.Repeat("x", 4096)}); err == nil {
		t.Fatal("accepted oversized environment entry")
	}
	if _, _, err := partitionExecEnvironment(map[string]string{"BAD-NAME": "value"}, []string{"BAD-NAME"}); err == nil {
		t.Fatal("accepted invalid environment key")
	}
}

type secretStagingRuntime struct {
	*lifecycleRuntime
	stageSpec  runtime.ExecSpec
	stageInput []byte
	stageErr   error
}

func (fake *secretStagingRuntime) Exec(ctx context.Context, snapshot runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	if len(spec.Argv) >= 2 && spec.Argv[1] == "stage-env" {
		fake.stageSpec = cloneRuntimeExecSpec(spec)
		fake.stageInput, _ = io.ReadAll(streams.Stdin)
		return runtime.Exit{}, fake.stageErr
	}
	return fake.lifecycleRuntime.Exec(ctx, snapshot, spec, streams)
}

func TestStageExecEnvironmentUsesGuestOwnedStdinAndCleansAfterFailure(t *testing.T) {
	injected := errors.New("stage failed")
	base := newLifecycleRuntime()
	fake := &secretStagingRuntime{lifecycleRuntime: base, stageErr: injected}
	service := &LifecycleService{runtime: fake, user: func() string { return "1000:1000" }}
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.stageExecEnvironment(context.Background(), runtime.ResourceSnapshot{}, runID, map[string]string{"TOKEN": "secret"}, []string{"TOKEN"})
	if !errors.Is(err, injected) {
		t.Fatalf("stageExecEnvironment() error = %v, want stage failure", err)
	}
	if len(base.copies) != 0 {
		t.Fatalf("secret used runtime copy: %#v", base.copies)
	}
	if got := string(fake.stageInput); got != "TOKEN=secret\x00" {
		t.Fatalf("stage stdin = %q", got)
	}
	arguments := strings.Join(fake.stageSpec.Argv, " ")
	if !strings.Contains(arguments, "dsx-guest stage-env --path /tmp/dsx-run/") || strings.Contains(arguments, "secret") || len(fake.stageSpec.Env) != 0 || fake.stageSpec.User != "1000:1000" {
		t.Fatalf("unsafe stage spec = %#v", fake.stageSpec)
	}
	destination := fake.stageSpec.Argv[len(fake.stageSpec.Argv)-1]
	if !strings.Contains(strings.Join(base.calls, "\n"), "exec:/usr/local/libexec/dsx/dsx-guest exec -- /bin/rm -rf -- "+destination) {
		t.Fatalf("guest cleanup absent: %#v", base.calls)
	}
}
