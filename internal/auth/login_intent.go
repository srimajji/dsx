package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/srimajji/dsx/internal/model"
)

const (
	AuthLoginIntentVersion = 1
	maxAuthLoginIntentBytes = 64 << 10
)

type AuthLoginIntentState string

const (
	AuthLoginPlanned  AuthLoginIntentState = "planned"
	AuthLoginCreating AuthLoginIntentState = "creating"
	AuthLoginRunning  AuthLoginIntentState = "running"
	AuthLoginCleaning AuthLoginIntentState = "cleaning"
)

// AuthLoginIntent is the write-ahead ownership record for a disposable,
// project-scoped auth-only runtime session. It contains no secret values or
// workspace identity.
type AuthLoginIntent struct {
	Version       int                  `json:"version"`
	Generation    uint64               `json:"generation"`
	Project       Project              `json:"project"`
	SessionID     string               `json:"session_id"`
	PlanHash      string               `json:"plan_hash"`
	State         AuthLoginIntentState `json:"state"`
	VolumeName    string               `json:"volume_name"`
	VolumeID      string               `json:"volume_id,omitempty"`
	ContainerName string               `json:"container_name"`
	ContainerID   string               `json:"container_id,omitempty"`
}

func ValidateAuthLoginIntent(intent AuthLoginIntent) error {
	if intent.Version != AuthLoginIntentVersion || intent.Generation == 0 {
		return errors.New("invalid auth login intent version or generation")
	}
	if err := validateProject(intent.Project); err != nil {
		return err
	}
	if _, err := model.ParseRunID(intent.SessionID); err != nil {
		return err
	}
	if err := validateDigest(intent.PlanHash); err != nil {
		return errors.New("invalid auth login plan hash")
	}
	switch intent.State {
	case AuthLoginPlanned, AuthLoginCreating, AuthLoginRunning, AuthLoginCleaning:
	default:
		return errors.New("invalid auth login intent state")
	}
	if !validAuthLoginResourceName(intent.VolumeName) || !validAuthLoginResourceName(intent.ContainerName) {
		return errors.New("invalid auth login resource names")
	}
	if strings.ContainsAny(intent.VolumeID+intent.ContainerID, "\x00\r\n") {
		return errors.New("invalid auth login runtime resource ID")
	}
	return nil
}

func validAuthLoginResourceName(value string) bool {
	if len(value) == 0 || len(value) > 62 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func (repository *Repository) CreateAuthLoginIntent(ctx context.Context, intent AuthLoginIntent) error {
	if err := ValidateAuthLoginIntent(intent); err != nil {
		return err
	}
	if intent.Generation != 1 || intent.State != AuthLoginPlanned || intent.VolumeID != "" || intent.ContainerID != "" {
		return errors.New("new auth login intent must be a planned generation-one record")
	}
	return repository.withFileLock(ctx, repository.authLoginIntentLock(intent.Project, intent.SessionID), func() error {
		path := repository.authLoginIntentPath(intent.Project, intent.SessionID)
		if _, err := os.Lstat(path); err == nil {
			return errors.New("auth login intent already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return repository.writeAuthLoginIntent(path, intent)
	})
}

func (repository *Repository) ReplaceAuthLoginIntent(ctx context.Context, intent AuthLoginIntent, expectedGeneration uint64) error {
	if err := ValidateAuthLoginIntent(intent); err != nil {
		return err
	}
	if intent.Generation != expectedGeneration+1 {
		return errors.New("auth login intent generation must advance by one")
	}
	return repository.withFileLock(ctx, repository.authLoginIntentLock(intent.Project, intent.SessionID), func() error {
		current, found, err := repository.loadAuthLoginIntent(intent.Project, intent.SessionID)
		if err != nil {
			return err
		}
		if !found || current.Generation != expectedGeneration {
			return errors.New("auth login intent changed concurrently")
		}
		if current.Project != intent.Project || current.SessionID != intent.SessionID || current.PlanHash != intent.PlanHash || current.VolumeName != intent.VolumeName || current.ContainerName != intent.ContainerName {
			return errors.New("auth login intent immutable authority changed")
		}
		return repository.writeAuthLoginIntent(repository.authLoginIntentPath(intent.Project, intent.SessionID), intent)
	})
}

func (repository *Repository) LoadAuthLoginIntent(ctx context.Context, project Project, sessionID string) (AuthLoginIntent, bool, error) {
	if err := ctx.Err(); err != nil {
		return AuthLoginIntent{}, false, err
	}
	if err := validateProject(project); err != nil {
		return AuthLoginIntent{}, false, err
	}
	if _, err := model.ParseRunID(sessionID); err != nil {
		return AuthLoginIntent{}, false, err
	}
	return repository.loadAuthLoginIntent(project, sessionID)
}

func (repository *Repository) DeleteAuthLoginIntent(ctx context.Context, project Project, sessionID string) error {
	if err := validateProject(project); err != nil {
		return err
	}
	if _, err := model.ParseRunID(sessionID); err != nil {
		return err
	}
	return repository.withFileLock(ctx, repository.authLoginIntentLock(project, sessionID), func() error {
		path := repository.authLoginIntentPath(project, sessionID)
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return syncDirectory(filepath.Dir(path))
	})
}

func (repository *Repository) loadAuthLoginIntent(project Project, sessionID string) (AuthLoginIntent, bool, error) {
	path := repository.authLoginIntentPath(project, sessionID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return AuthLoginIntent{}, false, nil
	}
	if err != nil {
		return AuthLoginIntent{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxAuthLoginIntentBytes {
		return AuthLoginIntent{}, false, errors.New("auth login intent is not a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return AuthLoginIntent{}, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxAuthLoginIntentBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return AuthLoginIntent{}, false, errors.Join(readErr, closeErr)
	}
	if len(data) > maxAuthLoginIntentBytes {
		return AuthLoginIntent{}, false, errors.New("auth login intent exceeds its size limit")
	}
	var intent AuthLoginIntent
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return AuthLoginIntent{}, false, errors.New("auth login intent is invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return AuthLoginIntent{}, false, errors.New("auth login intent contains trailing data")
	}
	if err := ValidateAuthLoginIntent(intent); err != nil {
		return AuthLoginIntent{}, false, err
	}
	return intent, true, nil
}

func (repository *Repository) writeAuthLoginIntent(path string, intent AuthLoginIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	if len(data) > maxAuthLoginIntentBytes {
		return errors.New("auth login intent exceeds its size limit")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".intent-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (repository *Repository) authLoginIntentPath(project Project, sessionID string) string {
	return filepath.Join(repository.root, "login-intents", string(project.ID), string(project.Harness), sessionID+".json")
}

func (repository *Repository) authLoginIntentLock(project Project, sessionID string) string {
	return filepath.Join(repository.root, "locks", "login-intents", string(project.ID), string(project.Harness), sessionID+".lock")
}
