package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/claude"
	"github.com/srimajji/dsx/internal/harness/codex"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

type authSessionRunnerStub struct {
	login func(context.Context, AuthSessionRequest) (AuthSessionResult, error)
}

func (runner authSessionRunnerStub) Login(ctx context.Context, request AuthSessionRequest) (AuthSessionResult, error) {
	return runner.login(ctx, request)
}

func TestAuthServiceStatusImportRefreshAndSecretFreeErrors(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	codexRoot := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(codexRoot, "auth.json")
	if err := os.WriteFile(credential, []byte(`{"tokens":{"access_token":"secret-one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexRoot, "config.toml"), []byte("never import"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := auth.NewHostDiscovery(home)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAuthService(repository, discovery, nil, codex.New(), claude.New())
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.Status(context.Background(), AuthStatusRequest{Root: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 2 || status.Agents[0].Agent != harness.Claude || status.Agents[1].Agent != harness.Codex {
		t.Fatalf("sorted auth status = %#v", status.Agents)
	}
	if status.Agents[0].HostImport != auth.HostImportLoginRequired || status.Agents[1].HostImport != auth.HostImportAvailable {
		t.Fatalf("host import states = %#v", status.Agents)
	}
	if _, err := service.Import(context.Background(), AuthImportRequest{Root: projectRoot, Agent: "codex"}); model.ErrorCodeOf(err) != model.CodeUnapproved {
		t.Fatalf("unapproved import error = %v", err)
	}
	if _, err := service.Import(context.Background(), AuthImportRequest{Root: projectRoot, Agent: "claude", Approved: true}); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("Claude import error = %v", err)
	}
	if _, err := service.Import(context.Background(), AuthImportRequest{Root: projectRoot, Agent: "codex", Approved: true}); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(context.Background(), AuthStatusRequest{Root: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Agents[1].Configured {
		t.Fatal("Codex canonical credentials are not configured after import")
	}

	if err := os.WriteFile(credential, []byte(`{"tokens":{"access_token":"secret-two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), AuthRefreshRequest{Root: projectRoot, Agent: "codex", Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "secret-target")
	if err := os.WriteFile(external, []byte("must-not-appear-in-error"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, credential); err != nil {
		t.Fatal(err)
	}
	_, err = service.Refresh(context.Background(), AuthRefreshRequest{Root: projectRoot, Agent: "codex", Approved: true})
	if err == nil || strings.Contains(err.Error(), "must-not-appear") || strings.Contains(err.Error(), external) {
		t.Fatalf("secret-bearing refresh error = %v", err)
	}
}

func TestAuthServiceClaudeLoginPersistsOnlyDSXSessionCredential(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("host-secret-must-not-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := auth.NewHostDiscovery(home)
	if err != nil {
		t.Fatal(err)
	}
	runner := authSessionRunnerStub{login: func(_ context.Context, request AuthSessionRequest) (AuthSessionResult, error) {
		if request.Agent != harness.Claude || request.CredentialRoot == "" {
			t.Fatalf("login session = %#v", request)
		}
		credential := `{"claudeAiOauth":{"accessToken":"dsx-access","refreshToken":"dsx-refresh","expiresAt":4102444800000,"scopes":["user:inference"]}}`
		if err := os.WriteFile(filepath.Join(request.CredentialRoot, ".credentials.json"), []byte(credential), 0o600); err != nil {
			return AuthSessionResult{}, err
		}
		code := 0
		return AuthSessionResult{Exit: runtime.Exit{Code: &code}}, nil
	}}
	service, err := NewAuthService(repository, discovery, runner, claude.New())
	if err != nil {
		t.Fatal(err)
	}
	request := AuthLoginRequest{Root: projectRoot, Agent: "claude", Approved: true, Interactive: true, Stdin: bytes.NewReader(nil), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, RunInteractive: func(context.Context, InteractiveChild) (runtime.Exit, error) {
		code := 0
		return runtime.Exit{Code: &code}, nil
	}}
	result, err := service.Login(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promotion.Digest == "" || result.Promotion.Conflict {
		t.Fatalf("login result = %#v", result)
	}
	status, err := service.Status(context.Background(), AuthStatusRequest{Root: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || !status.Agents[0].Configured || status.Agents[0].HostImport != auth.HostImportLoginRequired {
		t.Fatalf("Claude status = %#v", status.Agents)
	}
	if strings.Contains(result.Promotion.CandidateRoot, "host-secret") {
		t.Fatal("host Claude credential entered result")
	}
}

func TestAuthServicePurgeRejectsActiveWorkspaceCopy(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	codexRoot := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexRoot, "auth.json"), []byte(`{"tokens":{"access_token":"token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := auth.NewHostDiscovery(home)
	if err != nil {
		t.Fatal(err)
	}
	adapter := codex.New()
	service, err := NewAuthService(repository, discovery, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Import(context.Background(), AuthImportRequest{Root: projectRoot, Agent: "codex", Approved: true}); err != nil {
		t.Fatal(err)
	}
	projectID, err := projectIDForRoot(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := repository.AcquireWorkspace(context.Background(), auth.Workspace{ProjectID: projectID, Name: "active", Harness: harness.Codex}, model.RunID("00000000-0000-7000-8000-000000000111"), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Purge(context.Background(), AuthPurgeRequest{Root: projectRoot, Agent: "codex", Approved: true}); model.ErrorCodeOf(err) != model.CodeConflict || !errors.Is(err, auth.ErrActiveCopies) {
		t.Fatalf("active purge error = %v", err)
	}
	if err := repository.ReleaseWorkspace(context.Background(), copy); err != nil {
		t.Fatal(err)
	}
	if err := service.Purge(context.Background(), AuthPurgeRequest{Root: projectRoot, Agent: "codex", Approved: true}); err != nil {
		t.Fatal(err)
	}
}
