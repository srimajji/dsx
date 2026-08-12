package app

import (
	"reflect"
	"strings"
	"testing"
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
