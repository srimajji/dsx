package model

import (
	"testing"
	"time"
)

func TestIDProjectStable(t *testing.T) {
	first, err := NewProjectID("/Volumes/Dev/work/example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewProjectID("/Volumes/Dev/work/example")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != projectIDLength {
		t.Fatalf("unstable project IDs: %q %q", first, second)
	}
	if _, err := ParseProjectID(string(first)); err != nil {
		t.Fatalf("parse generated ID: %v", err)
	}
}

func TestIDRejectsInvalidValues(t *testing.T) {
	for _, root := range []string{"", "relative", "/tmp/../tmp/project"} {
		if _, err := NewProjectID(root); err == nil {
			t.Errorf("NewProjectID(%q) succeeded", root)
		}
	}
	for _, name := range []string{"", "Upper", "-bad", "bad-", "has space", "this-sandbox-name-is-far-too-long"} {
		if _, err := ParseSandboxName(name); err == nil {
			t.Errorf("ParseSandboxName(%q) succeeded", name)
		}
	}
}

func TestIDRunUUIDv7(t *testing.T) {
	value, err := NewRunID(time.UnixMilli(1_786_233_600_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRunID(string(value)); err != nil {
		t.Fatalf("parse generated run ID %q: %v", value, err)
	}
	if _, err := ParseRunID("00000000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("accepted UUIDv4 as UUIDv7")
	}
}
