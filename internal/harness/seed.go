package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func SeedArtifacts(ctx context.Context, request SeedRequest) error {
	if request.SourceRoot == "" || request.DestinationRoot == "" {
		return fmt.Errorf("seed source and destination roots are required")
	}
	if request.MaxArtifactBytes <= 0 {
		return fmt.Errorf("seed artifact size limit must be positive")
	}
	for _, artifact := range request.Artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !cleanRelative(artifact) {
			return fmt.Errorf("seed artifact %q must be a clean relative path", artifact)
		}
		source := filepath.Join(request.SourceRoot, filepath.FromSlash(artifact))
		destination := filepath.Join(request.DestinationRoot, filepath.FromSlash(artifact))
		if err := copyRegularFile(source, destination, request.MaxArtifactBytes); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("seed %s: %w", artifact, err)
		}
	}
	return nil
}

func copyRegularFile(source, destination string, maxArtifactBytes int64) error {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	if before.Size() > maxArtifactBytes {
		return fmt.Errorf("source exceeds size limit")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) {
		return fmt.Errorf("source changed while opening")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".dsx-seed-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	copied, copyErr := io.Copy(temporary, io.LimitReader(input, maxArtifactBytes+1))
	if copyErr != nil {
		temporary.Close()
		return copyErr
	}
	if copied > maxArtifactBytes {
		temporary.Close()
		return fmt.Errorf("source exceeds size limit")
	}
	after, err := input.Stat()
	if err != nil {
		temporary.Close()
		return err
	}
	if !os.SameFile(opened, after) || after.Size() > maxArtifactBytes || copied != after.Size() {
		temporary.Close()
		return fmt.Errorf("source changed during copy")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func IsRegularPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("permissions %04o expose credentials", info.Mode().Perm())
	}
	return nil
}
