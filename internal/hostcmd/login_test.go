package hostcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

func TestLoginPassesExplicitInteractiveRequestAndExit(t *testing.T) {
	code := 29
	service := &harnessStub{loginResult: app.HarnessLoginResult{Agent: harness.Codex, Version: "1.18.15", Exit: runtime.Exit{Code: &code}}}
	stdin := strings.NewReader("terminal input")
	opens := 0
	dispatcher := NewDispatcher(Dependencies{
		Harness: service,
		Stdin:   stdin,
		IsTTY:   func(io.Reader, io.Writer) bool { return true },
		LoginBrowser: func(context.Context, string) error {
			opens++
			return nil
		},
	})
	var stdout, stderr bytes.Buffer
	approval := strings.Repeat("a", 64)
	exit := dispatcher.Execute(context.Background(), []string{"login", "--agent", "codex", "--profile", "work", "--root", "/project", "--approve-config", approval}, &stdout, &stderr)
	if exit != code || stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(service.loginRequests) != 1 {
		t.Fatalf("login requests = %d", len(service.loginRequests))
	}
	request := service.loginRequests[0]
	if request.Agent != "codex" || request.Profile != "work" || request.Root != "/project" || request.ApproveConfig != approval || !request.Interactive {
		t.Fatalf("request = %#v", request)
	}
	if request.Stdin != stdin || request.Stdout != &stdout || request.Stderr != &stderr || request.RunInteractive == nil || request.OpenBrowser == nil {
		t.Fatalf("interactive request plumbing = %#v", request)
	}
	if err := request.OpenBrowser(context.Background(), "https://claude.ai/oauth/authorize?state=abcdefghijklmnop"); err != nil || opens != 1 {
		t.Fatalf("browser opener = %v, calls = %d", err, opens)
	}
	if stdout.String() != "Agent: \"codex\"\nVersion: \"1.18.15\"\n" {
		t.Fatalf("login status output = %q", stdout.String())
	}
}

func TestLoginRejectsNonTTYAndInvalidOrMissingArgumentsWithoutMutation(t *testing.T) {
	approval := strings.Repeat("b", 64)
	tests := [][]string{
		{"login", "--agent", "codex", "--profile", "default", "--root", "/project", "--approve-config", approval},
		{"login", "--profile", "default", "--root", "/project", "--approve-config", approval},
		{"login", "--agent", "codex", "--root", "/project", "--approve-config", approval},
		{"login", "--agent", "codex", "--profile", "default", "--approve-config", approval},
		{"login", "--agent", "unknown", "--profile", "default", "--root", "/project", "--approve-config", approval},
		{"login", "--agent", "codex", "--profile", "Upper", "--root", "/project", "--approve-config", approval},
		{"login", "--agent", "codex", "--profile", "default", "--root", "/project", "--approve-config", "stale"},
	}
	for index, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			service := &harnessStub{}
			isTTY := func(io.Reader, io.Writer) bool { return true }
			if index == 0 {
				isTTY = func(io.Reader, io.Writer) bool { return false }
			}
			dispatcher := NewDispatcher(Dependencies{Harness: service, Stdin: strings.NewReader(""), IsTTY: isTTY})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if len(service.loginRequests) != 0 {
				t.Fatalf("service mutated with requests %#v", service.loginRequests)
			}
		})
	}
}

func TestLoginPreservesSignalAndReportsServiceFailure(t *testing.T) {
	approval := strings.Repeat("c", 64)
	args := []string{"login", "--agent", "omp", "--profile", "default", "--root", "/project", "--approve-config", approval}
	service := &harnessStub{loginResult: app.HarnessLoginResult{Exit: runtime.Exit{Signal: "SIGTERM"}}}
	dispatcher := NewDispatcher(Dependencies{Harness: service, Stdin: strings.NewReader(""), IsTTY: func(io.Reader, io.Writer) bool { return true }})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 143 {
		t.Fatalf("signal exit = %d, stderr = %q", exit, stderr.String())
	}
	service.err = errors.New("provider refused login")
	stderr.Reset()
	if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 1 || !strings.Contains(stderr.String(), "provider refused login") {
		t.Fatalf("failure exit = %d, stderr = %q", exit, stderr.String())
	}
}
func TestLoginClaimsOneCommandSignalOwnerAndReturnsExactChildResult(t *testing.T) {
	for _, test := range []struct {
		name        string
		signalCount int
		want        int
	}{
		{name: "single-signal", signalCount: 1, want: 42},
		{name: "double-signal", signalCount: 2, want: 137},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := &signalReadyWriter{ready: make(chan struct{})}
			state := &terminalStateStub{}
			service := &harnessStub{}
			service.loginFunc = func(ctx context.Context, request app.HarnessLoginRequest) (app.HarnessLoginResult, error) {
				exit, err := request.RunInteractive(ctx, app.InteractiveChild{
					Argv:   []string{"/bin/sh", "-c", "trap 'sleep 0.2; exit 42' INT; echo ready; while :; do :; done"},
					Stdout: output,
				})
				return app.HarnessLoginResult{Agent: harness.OMP, Version: "0.40.0", Exit: exit}, err
			}
			dispatcher := NewDispatcher(Dependencies{
				Harness:       service,
				Stdin:         strings.NewReader(""),
				IsTTY:         func(io.Reader, io.Writer) bool { return true },
				TerminalState: state,
			})
			ctx, stopSignals := terminal.CommandSignalContext(context.Background())
			defer stopSignals()
			result := make(chan int, 1)
			go func() {
				approval := strings.Repeat("d", 64)
				result <- dispatcher.Execute(ctx, []string{"login", "--agent", "omp", "--profile", "default", "--root", "/project", "--approve-config", approval}, io.Discard, io.Discard)
			}()
			select {
			case <-output.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("login child did not become ready")
			}
			for index := range test.signalCount {
				if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
				if index+1 < test.signalCount {
					time.Sleep(50 * time.Millisecond)
				}
			}
			select {
			case exit := <-result:
				if exit != test.want {
					t.Fatalf("dispatcher exit = %d, want exact child exit %d", exit, test.want)
				}
				if state.releases != 1 || state.restores != 1 {
					t.Fatalf("release/restore = %d/%d", state.releases, state.restores)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("login child did not receive signal")
			}
		})
	}
}

func TestLoginSetupSignalCancelsBeforeInteractiveChild(t *testing.T) {
	entered := make(chan struct{})
	state := &terminalStateStub{}
	service := &harnessStub{
		loginFunc: func(ctx context.Context, request app.HarnessLoginRequest) (app.HarnessLoginResult, error) {
			if request.RunInteractive == nil {
				return app.HarnessLoginResult{}, errors.New("interactive runner is missing")
			}
			close(entered)
			<-ctx.Done()
			return app.HarnessLoginResult{}, ctx.Err()
		},
	}
	dispatcher := NewDispatcher(Dependencies{
		Harness:       service,
		Stdin:         strings.NewReader(""),
		IsTTY:         func(io.Reader, io.Writer) bool { return true },
		TerminalState: state,
	})
	ctx, stopSignals := terminal.CommandSignalContext(context.Background())
	defer stopSignals()
	result := make(chan int, 1)
	go func() {
		approval := strings.Repeat("e", 64)
		result <- dispatcher.Execute(ctx, []string{"login", "--agent", "omp", "--profile", "default", "--root", "/project", "--approve-config", approval}, io.Discard, io.Discard)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("login setup did not start")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-result:
		if exit != 1 {
			t.Fatalf("dispatcher exit = %d, want cancellation failure", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SIGINT did not cancel login setup")
	}
	if state.releases != 0 || state.restores != 0 {
		t.Fatalf("terminal changed before login child: release/restore = %d/%d", state.releases, state.restores)
	}
}

func TestLoginHelpIsExplicitAndTopLevelHelpListsRoute(t *testing.T) {
	for _, args := range [][]string{{"login", "--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if exit := Execute(context.Background(), args, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", args, exit, stderr.String())
		}
		if !strings.Contains(stdout.String(), "dsx login") || !strings.Contains(stdout.String(), "--approve-config") {
			t.Fatalf("args = %#v, help = %q", args, stdout.String())
		}
	}
}
