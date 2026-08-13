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

func TestHostAWSMirrorPublishesOnlyDefaultAndCompleteRotations(t *testing.T) {
	config := "[default]\nregion = eu-west-1\n[named]\nregion = us-east-1\n"
	credentials := hostAWSTemporaryCredentials("one") + "\n[named]\naws_access_key_id = named-access\naws_secret_access_key = named-secret\naws_session_token = named-token\n"
	source := hostAWSFixture(t, config, credentials)
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := validatedLeappMirrorSpec(authority)
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(canonicalTemporaryDirectory(t), hostAWSWorkspaceDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	configDigest, credentialDigest, code, err := synchronizeLeappMirror(mirror, spec, [32]byte{}, [32]byte{})
	if err != nil || code != "" {
		t.Fatalf("initial synchronize = %q, %v", code, err)
	}
	for name, forbidden := range map[string]string{hostAWSConfigFile: "[named]", hostAWSCredentialsFile: "named-secret"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(contents, []byte(forbidden)) || !bytes.Contains(contents, []byte("[default]")) {
			t.Fatalf("mirror %s did not contain only default: %q", name, contents)
		}
	}

	nextConfig := "[default]\nregion = ap-southeast-2\n[ignored]\nregion = eu-central-1\n"
	nextCredentials := hostAWSTemporaryCredentials("two") + "\n[ignored]\naws_access_key_id = ignored-access\naws_secret_access_key = ignored-secret\naws_session_token = ignored-token\n"
	writeHostAWSFiles(t, source, nextConfig, nextCredentials)
	configDigest, credentialDigest, code, err = synchronizeLeappMirror(mirror, spec, configDigest, credentialDigest)
	if err != nil || code != "" {
		t.Fatalf("replacement synchronize = %q, %v", code, err)
	}
	filtered, state, err := FilterHostDefaultSnapshot(HostAWSDirectorySnapshot{Config: []byte(nextConfig), Credentials: []byte(nextCredentials)})
	if err != nil || state != HostDefaultAvailable {
		t.Fatalf("filter replacement = %q, %v", state, err)
	}
	for name, want := range map[string][]byte{hostAWSConfigFile: filtered.Config, hostAWSCredentialsFile: filtered.Credentials} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || !bytes.Equal(contents, want) {
			t.Fatalf("replacement %s was partial or unfiltered: got %q want %q, %v", name, contents, want, readErr)
		}
		info, statErr := os.Lstat(filepath.Join(mirror, leappMirrorCurrentName, name))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			t.Fatalf("mirror %s mode = %v, %v", name, info, statErr)
		}
	}
}
func TestHostAWSMirrorGenerationCommitPreservesLastKnownGoodOnFailure(t *testing.T) {
	mirror := filepath.Join(canonicalTemporaryDirectory(t), hostAWSWorkspaceDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	old := HostAWSDirectorySnapshot{Config: []byte("config-one"), Credentials: []byte("credentials-one")}
	if err := publishLeappMirrorGeneration(mirror, old, nil); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected commit failure")
	next := HostAWSDirectorySnapshot{Config: []byte("config-two"), Credentials: []byte("credentials-two")}
	err := publishLeappMirrorGeneration(mirror, next, func() error {
		config, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, hostAWSConfigFile))
		if readErr != nil || string(config) != "config-one" {
			t.Fatalf("current generation changed before commit: %q, %v", config, readErr)
		}
		credentials, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, hostAWSCredentialsFile))
		if readErr != nil || string(credentials) != "credentials-one" {
			t.Fatalf("current generation was mixed before commit: %q, %v", credentials, readErr)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("publish failure = %v", err)
	}
	for name, want := range map[string]string{hostAWSConfigFile: "config-one", hostAWSCredentialsFile: "credentials-one"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("last known-good %s = %q, %v", name, contents, readErr)
		}
	}
	if err := publishLeappMirrorGeneration(mirror, next, func() error {
		for name, want := range map[string]string{hostAWSConfigFile: "config-one", hostAWSCredentialsFile: "credentials-one"} {
			contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
			if readErr != nil || string(contents) != want {
				t.Fatalf("pre-commit generation mixed at %s: %q, %v", name, contents, readErr)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{hostAWSConfigFile: "config-two", hostAWSCredentialsFile: "credentials-two"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("committed generation mixed at %s: %q, %v", name, contents, readErr)
		}
	}
}

func TestHostAWSMirrorTransientUnsafeReadPreservesLastKnownGood(t *testing.T) {
	mirror := filepath.Join(canonicalTemporaryDirectory(t), hostAWSWorkspaceDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	initial := HostAWSDirectorySnapshot{Config: []byte("config-one"), Credentials: []byte("credentials-one")}
	if err := publishLeappMirrorGeneration(mirror, initial, nil); err != nil {
		t.Fatal(err)
	}
	oldConfig := sha256.Sum256(initial.Config)
	oldCredentials := sha256.Sum256(initial.Credentials)
	reads := 0
	nextConfig, nextCredentials, code, err := synchronizeLeappMirrorWithSnapshot(mirror, leappMirrorSpec{}, oldConfig, oldCredentials, func() (HostAWSDirectorySnapshot, string, error) {
		reads++
		return HostAWSDirectorySnapshot{}, "source_unsafe", ErrHostAWSSourceUnsafe
	})
	if !errors.Is(err, ErrHostAWSSourceUnsafe) || code != "source_unsafe" || reads != 1 || nextConfig != oldConfig || nextCredentials != oldCredentials {
		t.Fatalf("transient read = digests %x/%x code %q err %v reads %d", nextConfig, nextCredentials, code, err, reads)
	}
	for name, want := range map[string]string{hostAWSConfigFile: "config-one", hostAWSCredentialsFile: "credentials-one"} {
		contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("transient read replaced last-known-good %s: %q, %v", name, contents, readErr)
		}
	}
}

func TestHostAWSMirrorStableRevocationPublishesEmptyGeneration(t *testing.T) {
	for _, test := range []struct {
		name        string
		config      string
		credentials string
	}{
		{name: "unavailable", config: "[named]\nregion = us-east-1\n", credentials: "[named]\naws_access_key_id = named\naws_secret_access_key = named\naws_session_token = named\n"},
		{name: "unsupported", config: "[default]\ncredential_process = external-command\n", credentials: hostAWSTemporaryCredentials("unsupported")},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := hostAWSFixture(t, "[default]\nregion = eu-west-1\n", hostAWSTemporaryCredentials("available"))
			authority, err := ResolveHostAWSDirectory(source)
			if err != nil {
				t.Fatal(err)
			}
			spec, _, err := validatedLeappMirrorSpec(authority)
			if err != nil {
				t.Fatal(err)
			}
			mirror := filepath.Join(canonicalTemporaryDirectory(t), hostAWSWorkspaceDataName)
			if err := os.Mkdir(mirror, 0o700); err != nil {
				t.Fatal(err)
			}
			configDigest, credentialsDigest, code, err := synchronizeLeappMirror(mirror, spec, [32]byte{}, [32]byte{})
			if err != nil || code != "" {
				t.Fatalf("initial synchronize = %q, %v", code, err)
			}
			previousGeneration, err := currentLeappMirrorGeneration(mirror)
			if err != nil || previousGeneration == "" {
				t.Fatalf("initial generation = %q, %v", previousGeneration, err)
			}
			writeHostAWSFiles(t, source, test.config, test.credentials)
			_, _, code, err = synchronizeLeappMirror(mirror, spec, configDigest, credentialsDigest)
			if err != nil || code != "" {
				t.Fatalf("revocation synchronize = %q, %v", code, err)
			}
			for _, name := range []string{hostAWSConfigFile, hostAWSCredentialsFile} {
				contents, readErr := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, name))
				if readErr != nil || len(contents) != 0 {
					t.Fatalf("revoked %s retained host bytes: %q, %v", name, contents, readErr)
				}
			}
			if _, statErr := os.Lstat(previousGeneration); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("revocation retained prior credential generation %q: %v", previousGeneration, statErr)
			}
		})
	}
}

func TestHostAWSMirrorReplacementPropagationBudget(t *testing.T) {
	source := hostAWSFixture(t, "[default]\nregion = eu-west-1\n", hostAWSTemporaryCredentials("one"))
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := validatedLeappMirrorSpec(authority)
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(canonicalTemporaryDirectory(t), hostAWSWorkspaceDataName)
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	configDigest, credentialsDigest, code, err := synchronizeLeappMirror(mirror, spec, [32]byte{}, [32]byte{})
	if err != nil || code != "" {
		t.Fatalf("initial synchronize = %q, %v", code, err)
	}
	initialGeneration, err := currentLeappMirrorGeneration(mirror)
	if err != nil || initialGeneration == "" {
		t.Fatalf("initial generation = %q, %v", initialGeneration, err)
	}
	unchangedStarted := time.Now()
	for range 32 {
		configDigest, credentialsDigest, code, err = synchronizeLeappMirror(mirror, spec, configDigest, credentialsDigest)
		if err != nil || code != "" {
			t.Fatalf("unchanged synchronize = %q, %v", code, err)
		}
	}
	unchangedElapsed := time.Since(unchangedStarted)
	unchangedGeneration, err := currentLeappMirrorGeneration(mirror)
	if err != nil || unchangedGeneration != initialGeneration {
		t.Fatalf("unchanged polling republished generation: initial %q current %q, %v", initialGeneration, unchangedGeneration, err)
	}

	replacement := hostAWSTemporaryCredentials("replacement")
	writeHostAWSFiles(t, source, "[default]\nregion = ap-southeast-2\n", replacement)
	replacementStarted := time.Now()
	_, _, code, err = synchronizeLeappMirror(mirror, spec, configDigest, credentialsDigest)
	replacementElapsed := time.Since(replacementStarted)
	if err != nil || code != "" {
		t.Fatalf("replacement synchronize = %q, %v", code, err)
	}
	if leappMirrorPollInterval+replacementElapsed >= 2*time.Second {
		t.Fatalf("replacement propagation exceeded budget: poll %s + filter/publish %s", leappMirrorPollInterval, replacementElapsed)
	}
	replacementGeneration, err := currentLeappMirrorGeneration(mirror)
	if err != nil || replacementGeneration == "" || replacementGeneration == initialGeneration {
		t.Fatalf("replacement generation = %q after %q, %v", replacementGeneration, initialGeneration, err)
	}
	credentials, err := os.ReadFile(filepath.Join(mirror, leappMirrorCurrentName, hostAWSCredentialsFile))
	if err != nil || !bytes.Contains(credentials, []byte("replacement")) {
		t.Fatalf("replacement was not published: %q, %v", credentials, err)
	}
	t.Logf("32 unchanged filter passes: %s; replacement filter/publish: %s; worst polling interval: %s", unchangedElapsed, replacementElapsed, leappMirrorPollInterval)
}

func TestHostAWSMirrorSourceFailuresAreNonSecret(t *testing.T) {
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
			writeHostAWSFiles(t, source, "replacement", "replacement-secret-must-not-appear")
		}},
		{name: "credential symlink", want: "source_unsafe", mutate: func(t *testing.T, source string) {
			if err := os.Remove(filepath.Join(source, hostAWSCredentialsFile)); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(source), "outside-secret")
			if err := os.WriteFile(target, []byte("symlink-secret-must-not-appear"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(source, hostAWSCredentialsFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized credential", want: "source_oversized", mutate: func(t *testing.T, source string) {
			temporary := filepath.Join(source, ".credentials-oversized")
			file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(MaxHostAWSFileBytes + 1); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, filepath.Join(source, hostAWSCredentialsFile)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := hostAWSFixture(t, "[default]\nregion = eu-west-1\n", hostAWSTemporaryCredentials("approved-secret-must-not-appear"))
			authority, err := ResolveHostAWSDirectory(source)
			if err != nil {
				t.Fatal(err)
			}
			spec, _, err := validatedLeappMirrorSpec(authority)
			if err != nil {
				t.Fatal(err)
			}
			run := canonicalTemporaryDirectory(t)
			mirror := filepath.Join(run, hostAWSWorkspaceDataName)
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

func TestHostAWSWorkspaceDisableRecoversDeadHelperAndPreservesStableChannel(t *testing.T) {
	stateRoot := canonicalTemporaryDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionHostAWSWorkspaceManager(stateRoot, executable)
	if err != nil {
		t.Fatal(err)
	}
	identity := leaseTestIdentity(t, "host-aws-permissions", "01890f5c-7b00-7000-8000-000000000071")
	stablePath, err := manager.Prepare(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	paths, exists, err := manager.existingPaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("prepared Leapp mirror paths do not exist")
	}
	if err := publishLeappMirrorGeneration(paths.mirror, HostAWSDirectorySnapshot{Config: []byte("config"), Credentials: []byte("credentials")}, nil); err != nil {
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
	if err := manager.Disable(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if stablePath != paths.mirror {
		t.Fatalf("stable path = %q, want %q", stablePath, paths.mirror)
	}
	if empty, err := hostAWSPublicationEmpty(stablePath); err != nil || !empty {
		t.Fatalf("disabled publication empty = %v, %v", empty, err)
	}
	if absent, err := exactLeappMirrorHelperArtifactsAbsent(paths); err != nil || !absent {
		t.Fatalf("disabled helper artifacts absent = %v, %v", absent, err)
	}
	if preparedAgain, err := manager.Prepare(context.Background(), identity); err != nil || preparedAgain != stablePath {
		t.Fatalf("idempotent prepare path = %q, %v", preparedAgain, err)
	}
}
func TestHostAWSWorkspaceStablePathAcrossEnableDisableReenableAndExactRemove(t *testing.T) {
	stateRoot := canonicalTemporaryDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionHostAWSWorkspaceManager(stateRoot, executable)
	if err != nil {
		t.Fatal(err)
	}
	identity := leaseTestIdentity(t, "host-aws-lifecycle", "01890f5c-7b00-7000-8000-000000000072")
	source := hostAWSFixture(t, "[default]\nregion=us-east-1\n", "[default]\naws_access_key_id=one\naws_secret_access_key=two\n")
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	manager.launchOverride = func(_ context.Context, paths leappMirrorPaths, gotIdentity LeaseIdentity, _ leappMirrorSpec, _ string) (string, error) {
		if gotIdentity != identity {
			t.Fatalf("enable identity = %#v, want %#v", gotIdentity, identity)
		}
		if err := publishLeappMirrorGeneration(paths.mirror, HostAWSDirectorySnapshot{Config: []byte("enabled-config"), Credentials: []byte("enabled-credentials")}, nil); err != nil {
			return "", err
		}
		return paths.mirror, nil
	}
	prepared, err := manager.Prepare(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	preparedAgain, err := manager.Prepare(context.Background(), identity)
	if err != nil || preparedAgain != prepared {
		t.Fatalf("idempotent Prepare() = %q, %v; want %q", preparedAgain, err, prepared)
	}
	enabled, err := manager.Enable(context.Background(), identity, authority)
	if err != nil || enabled != prepared {
		t.Fatalf("Enable() = %q, %v; want stable %q", enabled, err, prepared)
	}
	if err := manager.Disable(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if empty, err := hostAWSPublicationEmpty(prepared); err != nil || !empty {
		t.Fatalf("disabled publication empty = %v, %v", empty, err)
	}
	reenabled, err := manager.Enable(context.Background(), identity, authority)
	if err != nil || reenabled != prepared {
		t.Fatalf("re-Enable() = %q, %v; want stable %q", reenabled, err, prepared)
	}
	if err := manager.Remove(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(prepared); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed stable publication still exists: %v", err)
	}
	if err := manager.Remove(context.Background(), identity); err != nil {
		t.Fatalf("idempotent Remove() = %v", err)
	}
}

func TestHostAWSWorkspaceFirstEnableUnavailableFailsAndKeepsPreparedEmpty(t *testing.T) {
	stateRoot := canonicalTemporaryDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionHostAWSWorkspaceManager(stateRoot, executable)
	if err != nil {
		t.Fatal(err)
	}
	identity := leaseTestIdentity(t, "host-aws-unavailable", "01890f5c-7b00-7000-8000-000000000074")
	source := hostAWSFixture(t, "", "")
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	stablePath, err := manager.Prepare(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	manager.launchOverride = func(_ context.Context, paths leappMirrorPaths, _ LeaseIdentity, spec leappMirrorSpec, _ string) (string, error) {
		_, _, code, syncErr := synchronizeInitialLeappMirror(paths.mirror, spec)
		if syncErr == nil || code != "source_unavailable" {
			t.Fatalf("initial unavailable sync = %q, %v", code, syncErr)
		}
		return "", syncErr
	}
	if _, err := manager.Enable(context.Background(), identity, authority); err == nil {
		t.Fatal("first Enable() accepted unavailable host default")
	}
	if empty, verifyErr := hostAWSPublicationEmpty(stablePath); verifyErr != nil || !empty {
		t.Fatalf("failed first enable publication empty = %v, %v", empty, verifyErr)
	}
}

func TestHostAWSWorkspaceRemovePreservesAmbiguousPublicationEvidence(t *testing.T) {
	stateRoot := canonicalTemporaryDirectory(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProductionHostAWSWorkspaceManager(stateRoot, executable)
	if err != nil {
		t.Fatal(err)
	}
	identity := leaseTestIdentity(t, "host-aws-ambiguity", "01890f5c-7b00-7000-8000-000000000073")
	stablePath, err := manager.Prepare(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := filepath.Join(stablePath, "unowned")
	if err := os.WriteFile(ambiguous, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(context.Background(), identity); model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Remove() error = %v, want ambiguous", err)
	}
	if contents, err := os.ReadFile(ambiguous); err != nil || string(contents) != "preserve" {
		t.Fatalf("ambiguous evidence was not preserved: %q, %v", contents, err)
	}
}

func TestLeappMirrorExactReattachAndCrossIdentityControl(t *testing.T) {
	identity := leaseTestIdentity(t, "leapp-reattach", "01890f5c-7b00-7000-8000-000000000041")
	other := identity
	other.RunID = model.RunID("01890f5c-7b00-7000-8000-000000000042")
	spec := leappMirrorSpec{CanonicalPath: "/private/source", Source: HostAWSSourceIdentity{Device: 1, Inode: 2, UID: uint32(os.Geteuid())}}
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

func TestHostAWSDescriptorSnapshotRetriesPairedRotationWithoutMixedGeneration(t *testing.T) {
	source := hostAWSFixture(t, "config-one", "credentials-one")
	authority, err := ResolveHostAWSDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenApprovedHostAWSDirectory(authority)
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
			name, contents = hostAWSConfigFile, "config-two"
		case 3:
			name, contents = hostAWSCredentialsFile, "credentials-two"
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
	identity := leaseTestIdentity(t, "leapp-readiness", "01890f5c-7b00-7000-8000-000000000061")
	spec := leappMirrorSpec{CanonicalPath: "/private/source", Source: HostAWSSourceIdentity{Device: 1, Inode: 2, UID: uint32(os.Geteuid())}}
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
			manager := &ProductionHostAWSWorkspaceManager{
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
			if info, err := os.Stat(run); err != nil || !info.IsDir() {
				t.Fatalf("stable publication run was removed during failed-launch cleanup: %v", err)
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
