package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/model"
)

func TestLeappMirrorSynchronizesCompleteAtomicRotations(t *testing.T) {
	source := leappFixture(t, "config-one", strings.Repeat("generation-one\n", 256))
	authority, err := ResolveLeappDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := validatedLeappMirrorSpec(authority)
	if err != nil {
		t.Fatal(err)
	}
	run := canonicalTemporaryDirectory(t)
	mirror := filepath.Join(run, leappMirrorDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	configDigest, credentialDigest, code, err := synchronizeLeappMirror(mirror, spec, [32]byte{}, [32]byte{})
	if err != nil || code != "" {
		t.Fatalf("initial synchronize = %q, %v", code, err)
	}
	for index, generation := range []string{strings.Repeat("generation-two\n", 256), strings.Repeat("generation-three\n", 256)} {
		temporary := filepath.Join(source, ".credentials-next")
		if err := os.WriteFile(temporary, []byte(generation), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(source, leappCredentialsFile)); err != nil {
			t.Fatal(err)
		}
		configDigest, credentialDigest, code, err = synchronizeLeappMirror(mirror, spec, configDigest, credentialDigest)
		if err != nil || code != "" {
			t.Fatalf("rotation %d synchronize = %q, %v", index, code, err)
		}
		contents, err := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, leappCredentialsFile))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != generation {
			t.Fatalf("rotation %d was partial: got %d want %d", index, len(contents), len(generation))
		}
		info, err := os.Lstat(filepath.Join(mirror, leappMirrorCurrentName, leappCredentialsFile))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			t.Fatalf("mirror credentials mode = %v, %v", info, err)
		}
	}
}
func TestLeappMirrorGenerationCommitPreservesLastKnownGoodOnFailure(t *testing.T) {
	mirror := filepath.Join(canonicalTemporaryDirectory(t), leappMirrorDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	old := LeappDirectorySnapshot{Config: []byte("config-one"), Credentials: []byte("credentials-one")}
	if err := publishLeappMirrorGeneration(mirror, old, nil); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected commit failure")
	next := LeappDirectorySnapshot{Config: []byte("config-two"), Credentials: []byte("credentials-two")}
	err := publishLeappMirrorGeneration(mirror, next, func() error {
		config, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, leappConfigFile))
		if readErr != nil || string(config) != "config-one" {
			t.Fatalf("current generation changed before commit: %q, %v", config, readErr)
		}
		credentials, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, leappCredentialsFile))
		if readErr != nil || string(credentials) != "credentials-one" {
			t.Fatalf("current generation was mixed before commit: %q, %v", credentials, readErr)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("publish failure = %v", err)
	}
	for name, want := range map[string]string{leappConfigFile: "config-one", leappCredentialsFile: "credentials-one"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("last known-good %s = %q, %v", name, contents, readErr)
		}
	}
	if err := publishLeappMirrorGeneration(mirror, next, func() error {
		for name, want := range map[string]string{leappConfigFile: "config-one", leappCredentialsFile: "credentials-one"} {
			contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
			if readErr != nil || string(contents) != want {
				t.Fatalf("pre-commit generation mixed at %s: %q, %v", name, contents, readErr)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{leappConfigFile: "config-two", leappCredentialsFile: "credentials-two"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("committed generation mixed at %s: %q, %v", name, contents, readErr)
		}
	}
}

func TestLeappMirrorPairedSourceRotationRetriesThenFailCloses(t *testing.T) {
	mirror := filepath.Join(canonicalTemporaryDirectory(t), leappMirrorDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	initial := LeappDirectorySnapshot{Config: []byte("config-one"), Credentials: []byte("credentials-one")}
	if err := publishLeappMirrorGeneration(mirror, initial, nil); err != nil {
		t.Fatal(err)
	}
	oldConfig := sha256.Sum256(initial.Config)
	oldCredentials := sha256.Sum256(initial.Credentials)
	reads := 0
	_, _, code, err := synchronizeLeappMirrorWithSnapshot(mirror, leappMirrorSpec{}, oldConfig, oldCredentials, func() (LeappDirectorySnapshot, string, error) {
		reads++
		return LeappDirectorySnapshot{Config: []byte("config-two"), Credentials: []byte("credentials-one")}, "", nil
	})
	if err == nil || code != "source_unsafe" || reads != 3 {
		t.Fatalf("stable mixed source = code %q err %v reads %d", code, err, reads)
	}
	for name, want := range map[string]string{leappConfigFile: "config-one", leappCredentialsFile: "credentials-one"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("failed source rotation replaced %s: %q, %v", name, contents, readErr)
		}
	}
}

func TestLeappMirrorSourceFailuresAreNonSecretAndFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{name: "directory swap", want: "source_identity_changed", mutate: func(t *testing.T, source string) {
			if err := os.Rename(source, source+".approved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			writeLeappFiles(t, source, "replacement", "replacement-secret-must-not-appear")
		}},
		{name: "credential symlink", want: "source_unsafe", mutate: func(t *testing.T, source string) {
			if err := os.Remove(filepath.Join(source, leappCredentialsFile)); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(source), "outside-secret")
			if err := os.WriteFile(target, []byte("symlink-secret-must-not-appear"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(source, leappCredentialsFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized credential", want: "source_oversized", mutate: func(t *testing.T, source string) {
			temporary := filepath.Join(source, ".credentials-oversized")
			file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(MaxLeappFileBytes + 1); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, filepath.Join(source, leappCredentialsFile)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := leappFixture(t, "config", "approved-secret-must-not-appear")
			authority, err := ResolveLeappDirectory(source)
			if err != nil {
				t.Fatal(err)
			}
			spec, _, err := validatedLeappMirrorSpec(authority)
			if err != nil {
				t.Fatal(err)
			}
			run := canonicalTemporaryDirectory(t)
			mirror := filepath.Join(run, leappMirrorDataName)
			if err := os.Mkdir(mirror, 0o700); err != nil {
				t.Fatal(err)
			}
			configDigest, credentialDigest, _, err := synchronizeLeappMirror(mirror, spec, [32]byte{}, [32]byte{})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, source)
			_, _, code, syncErr := synchronizeLeappMirror(mirror, spec, configDigest, credentialDigest)
			if syncErr == nil || code != test.want {
				t.Fatalf("failure = %q, %v", code, syncErr)
			}
			if strings.Contains(syncErr.Error(), "approved-secret") || strings.Contains(syncErr.Error(), "replacement-secret") || strings.Contains(syncErr.Error(), "symlink-secret") {
				t.Fatalf("failure exposed credential contents: %v", syncErr)
			}
			if err := removeLeappMirrorDirectory(mirror); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(mirror); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mirror survived fail-closed cleanup: %v", err)
			}
		})
	}
}

func TestLeappMirrorHelperCommandStartsInDedicatedSession(t *testing.T) {
	command := exec.Command("/usr/bin/true")
	configureLeappMirrorCommand(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid {
		t.Fatalf("Leapp mirror helper process attributes = %+v", command.SysProcAttr)
	}
}

func TestLeappMirrorStopRecoversProvenDeadHelperArtifacts(t *testing.T) {
	stateRoot, err := os.MkdirTemp("/private/tmp", "dsx-leapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(stateRoot, leappMirrorDirectoryName, "aaaaaaaaaaaaaaaaaaaa", "main"))
		_ = os.Remove(filepath.Join(stateRoot, leappMirrorDirectoryName, "aaaaaaaaaaaaaaaaaaaa"))
		_ = os.Remove(filepath.Join(stateRoot, leappMirrorDirectoryName))
		_ = os.Remove(stateRoot)
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionLeappMirrorManager(stateRoot, executable)
	if err != nil {
		t.Fatal(err)
	}
	identity := LeaseIdentity{ProjectID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), Sandbox: model.SandboxName("main"), RunID: model.RunID("01890f5c-7b00-7000-8000-000000000071")}
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := publishLeappMirrorGeneration(paths.mirror, LeappDirectorySnapshot{Config: []byte("config"), Credentials: []byte("credentials")}, nil); err != nil {
		t.Fatal(err)
	}
	partial, err := os.MkdirTemp(paths.mirror, leappMirrorGenerationPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteLeappMirrorFile(partial, leappConfigFile, []byte("partially staged")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.mirror, leappMirrorWritePrefix+"dead"), []byte("staged"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.mirror, leappMirrorCurrentPrefix+"dead"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := leappMirrorLedger{Version: 1, Identity: identity, SpecDigest: strings.Repeat("a", 64), PID: 424242, ProcessStartedAt: time.Now().UTC(), Executable: manager.executable}
	encoded, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWritePrivate(paths.ledger, encoded); err != nil {
		t.Fatal(err)
	}
	if err := atomicWritePrivate(paths.token, []byte(strings.Repeat("t", 43)+"\n")); err != nil {
		t.Fatal(err)
	}
	manager.verifySocket = func(path string) error {
		if path != paths.socket {
			t.Fatalf("verified socket %q, want %q", path, paths.socket)
		}
		return nil
	}
	manager.controlOverride = func(context.Context, leappMirrorPaths, leappMirrorCommand) (leappMirrorResponse, error) {
		return leappMirrorResponse{}, errors.New("helper crashed")
	}
	manager.processAlive = func(pid int) (bool, error) {
		if pid != ledger.PID {
			t.Fatalf("checked PID %d, want %d", pid, ledger.PID)
		}
		return false, nil
	}
	if err := manager.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.run); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead helper run survived recovery: %v", err)
	}
}

func TestLeappMirrorExactReattachAndCrossIdentityControl(t *testing.T) {
	identity := LeaseIdentity{ProjectID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), Sandbox: model.SandboxName("main"), RunID: model.RunID("01890f5c-7b00-7000-8000-000000000041")}
	other := identity
	other.RunID = model.RunID("01890f5c-7b00-7000-8000-000000000042")
	spec := leappMirrorSpec{CanonicalPath: "/private/source", Source: LeappSourceIdentity{Device: 1, Inode: 2, UID: uint32(os.Geteuid())}}
	executable := executableIdentity{Path: "/private/dsx", Device: 3, Inode: 4, Size: 5, UID: uint32(os.Geteuid())}
	ledger := leappMirrorLedger{Version: 1, Identity: identity, Spec: spec, SpecDigest: strings.Repeat("a", 64), PID: 123, ProcessStartedAt: time.Now().UTC(), Executable: executable}
	if err := validateLeappMirrorLedger(ledger, identity, spec, ledger.SpecDigest, executable); err != nil {
		t.Fatal(err)
	}
	response := leappMirrorResponse{Version: 1, State: "running", Identity: identity, SpecDigest: ledger.SpecDigest, PID: ledger.PID, Executable: executable}
	if err := validateLeappMirrorResponse(response, ledger, "running"); err != nil {
		t.Fatal(err)
	}
	response.Identity = other
	if err := validateLeappMirrorResponse(response, ledger, "running"); err == nil {
		t.Fatal("cross-identity reattach was accepted")
	}
	token := strings.Repeat("t", 43)
	request := leappMirrorCommand{Version: 1, Action: "stop", Identity: identity, SpecDigest: ledger.SpecDigest, Token: token}
	if !validLeappMirrorControlRequest(request, identity, ledger.SpecDigest, token) {
		t.Fatal("exact control was refused")
	}
	request.Identity = other
	if validLeappMirrorControlRequest(request, identity, ledger.SpecDigest, token) {
		t.Fatal("cross-identity control was accepted")
	}
}

func TestLeappDescriptorSnapshotRetriesPairedRotationWithoutMixedGeneration(t *testing.T) {
	source := leappFixture(t, "config-one", "credentials-one")
	authority, err := ResolveLeappDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenApprovedLeappDirectory(authority)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	attempt := 0
	snapshot, err := opened.snapshotWithHook(func() {
		attempt++
		var name, contents string
		switch attempt {
		case 1:
			name, contents = leappConfigFile, "config-two"
		case 3:
			name, contents = leappCredentialsFile, "credentials-two"
		default:
			return
		}
		temporary := filepath.Join(source, "."+name+"-next")
		if err := os.WriteFile(temporary, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Config) != "config-two" || string(snapshot.Credentials) != "credentials-two" {
		t.Fatalf("mixed paired generation: config %q credentials %q", snapshot.Config, snapshot.Credentials)
	}
}

func TestLeappMirrorAcceptedReadinessValidationFailureStopsExactHelper(t *testing.T) {
	identity := LeaseIdentity{ProjectID: model.ProjectID("aaaaaaaaaaaaaaaaaaaa"), Sandbox: model.SandboxName("main"), RunID: model.RunID("01890f5c-7b00-7000-8000-000000000061")}
	spec := leappMirrorSpec{CanonicalPath: "/private/source", Source: LeappSourceIdentity{Device: 1, Inode: 2, UID: uint32(os.Geteuid())}}
	executable := executableIdentity{Path: "/private/dsx", Device: 3, Inode: 4, Size: 5, UID: uint32(os.Geteuid())}
	digest := strings.Repeat("a", 64)
	ready := leappMirrorResponse{Version: 1, State: "ready", Identity: identity, SpecDigest: digest, PID: 123, Executable: executable}
	validLedger := leappMirrorLedger{Version: 1, Identity: identity, Spec: spec, SpecDigest: digest, PID: ready.PID, ProcessStartedAt: time.Now().UTC(), Executable: executable}
	for _, test := range []struct {
		name   string
		load   func(string) (leappMirrorLedger, bool, error)
		verify func(string) error
	}{
		{name: "ledger", load: func(string) (leappMirrorLedger, bool, error) {
			return leappMirrorLedger{}, false, errors.New("injected ledger failure")
		}, verify: func(string) error { return nil }},
		{name: "snapshot", load: func(string) (leappMirrorLedger, bool, error) { return validLedger, true, nil }, verify: func(string) error { return errors.New("injected snapshot failure") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := filepath.Join(canonicalTemporaryDirectory(t), "run")
			if err := os.Mkdir(run, 0o700); err != nil {
				t.Fatal(err)
			}
			paths := makeLeappMirrorPaths(filepath.Dir(filepath.Dir(filepath.Dir(run))), filepath.Dir(filepath.Dir(run)), filepath.Dir(run), run)
			stopped := false
			manager := &ProductionLeappMirrorManager{
				executable: executable, stopWait: time.Second, loadReadyLedger: test.load, verifySnapshot: test.verify,
				controlOverride: func(_ context.Context, _ leappMirrorPaths, request leappMirrorCommand) (leappMirrorResponse, error) {
					stopped = request.Action == "stop" && request.Identity == identity && request.SpecDigest == digest
					return leappMirrorResponse{Version: 1, State: "stopped", Identity: identity, SpecDigest: digest, PID: ready.PID, Executable: executable}, nil
				},
				artifactsAbsent: func(leappMirrorPaths) (bool, error) { return true, nil },
			}
			cause := manager.validateReadyLeappMirrorArtifacts(paths, identity, spec, digest, ready)
			if cause == nil {
				t.Fatal("injected validation failure was accepted")
			}
			var acceptance bytes.Buffer
			if got := manager.failAcceptedLeappMirrorLaunch(context.Background(), paths, identity, digest, "token", ready, &acceptance, cause); got == nil {
				t.Fatal("launch failure disappeared")
			}
			if !stopped || !bytes.Equal(acceptance.Bytes(), []byte{1}) {
				t.Fatalf("cleanup stop=%v acceptance=%v", stopped, acceptance.Bytes())
			}
			if _, err := os.Lstat(run); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("exact failed-launch run cleanup = %v", err)
			}
		})
	}
}

func TestLeappMirrorReadinessAcceptanceTimeoutFailsClosed(t *testing.T) {
	reader, writer := io.Pipe()
	if awaitLeappMirrorAcceptance(reader, 5*time.Millisecond) {
		t.Fatal("missing readiness acceptance was accepted")
	}
	_ = writer.Close()
	_ = reader.Close()
}
