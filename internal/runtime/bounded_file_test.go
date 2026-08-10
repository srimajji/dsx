package runtime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReceiveBoundedFileExactLimit(t *testing.T) {
	name := filepath.Join(t.TempDir(), "artifact")
	contents := []byte("12345678")
	err := ReceiveBoundedFile(HostPath(name), BoundedFileOptions{MaximumBytes: int64(len(contents)), RejectNUL: true, Mode: 0o600}, func(output io.Writer) error {
		_, err := output.Write(contents)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("received contents = %q, %v", got, err)
	}
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("received metadata = %v, %v", info, err)
	}
}

func TestReceiveBoundedFileRejectsOversizeNULAndPartialTransfer(t *testing.T) {
	tests := []struct {
		name   string
		stream func(io.Writer) error
		want   error
	}{
		{name: "size plus one", stream: func(output io.Writer) error { _, _ = output.Write([]byte("123456789")); return nil }, want: ErrBoundedFileTooLarge},
		{name: "NUL", stream: func(output io.Writer) error { _, err := output.Write([]byte{'a', 0, 'b'}); return err }, want: ErrBoundedFileNUL},
		{name: "partial stream failure", stream: func(output io.Writer) error {
			if _, err := output.Write([]byte("part")); err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		}, want: io.ErrUnexpectedEOF},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "artifact")
			err := ReceiveBoundedFile(HostPath(name), BoundedFileOptions{MaximumBytes: 8, RejectNUL: true, Mode: 0o600}, test.stream)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination survived: %v", err)
			}
		})
	}
}

func TestReceiveBoundedFileNeverReplacesExistingDestination(t *testing.T) {
	name := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(name, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ReceiveBoundedFile(HostPath(name), BoundedFileOptions{MaximumBytes: 8, Mode: 0o600}, func(output io.Writer) error {
		_, err := output.Write([]byte("new"))
		return err
	})
	if err == nil {
		t.Fatal("existing destination was replaced")
	}
	contents, err := os.ReadFile(name)
	if err != nil || string(contents) != "existing" {
		t.Fatalf("existing destination = %q, %v", contents, err)
	}
}
