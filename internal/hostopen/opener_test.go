package hostopen

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordingRunner struct {
	argv []string
	err  error
}

func (runner *recordingRunner) Run(_ context.Context, argv []string) error {
	runner.argv = append([]string(nil), argv...)
	return runner.err
}

func TestOpenerUsesFixedStructuredMacOSArgv(t *testing.T) {
	runner := &recordingRunner{}
	opener, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	providerURL := "https://claude.ai/oauth/authorize?state=abcdefghijklmnop&value=a%20b;touch%20/tmp/owned"
	if err := opener.Open(context.Background(), providerURL); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/usr/bin/open", providerURL}; !reflect.DeepEqual(runner.argv, want) {
		t.Fatalf("argv = %#v, want %#v", runner.argv, want)
	}
}

func TestOpenerRejectsUnsafeURLAndExpiredLease(t *testing.T) {
	runner := &recordingRunner{}
	opener, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"file:///tmp/steal", "javascript:alert(1)", "http://claude.ai/oauth/authorize", "https://user@claude.ai/path", "https://claude.ai/path#fragment"} {
		if err := opener.Open(context.Background(), candidate); err == nil {
			t.Fatalf("accepted URL %q", candidate)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := opener.Open(ctx, "https://claude.ai/oauth/authorize?state=abcdefghijklmnop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired lease error = %v", err)
	}
	if runner.argv != nil {
		t.Fatalf("rejected opener invoked runner: %#v", runner.argv)
	}
}
