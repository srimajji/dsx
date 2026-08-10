package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

const (
	maxStagedSecretEnvironmentBytes = 1 << 20
	secretEnvironmentCleanupTimeout = 30 * time.Second
)

type stagedExecEnvironment struct {
	ordinary []string
	guest    runtime.GuestPath
}

func (service *LifecycleService) stageExecEnvironment(ctx context.Context, snapshot runtime.ResourceSnapshot, runID model.RunID, environment map[string]string, secretKeys []string) (stagedExecEnvironment, error) {
	ordinary, secret, err := partitionExecEnvironment(environment, secretKeys)
	if err != nil {
		return stagedExecEnvironment{}, model.Wrap(model.CodeInvalidInput, "validate shell environment", err)
	}
	staged := stagedExecEnvironment{ordinary: harnessEnvironment(ordinary)}
	if len(secret) == 0 {
		return staged, nil
	}
	contents, err := encodeSecretEnvironment(secret)
	if err != nil {
		return stagedExecEnvironment{}, model.Wrap(model.CodeInvalidInput, "encode secret environment", err)
	}
	guestName, err := secretEnvironmentGuestPath(runID)
	if err != nil {
		return stagedExecEnvironment{}, model.Wrap(model.CodeInternal, "allocate secret environment path", err)
	}
	stageSpec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "stage-env", "--path", string(guestName)},
		WorkingDir: workspaceGuestRoot,
		User:       service.user(),
	}
	exit, stageErr := service.runtime.Exec(ctx, snapshot, stageSpec, runtime.ExecIO{Stdin: bytes.NewReader(contents)})
	if stageErr == nil && (exit.Code == nil || *exit.Code != 0 || exit.Signal != "") {
		stageErr = model.NewError(model.CodeUnavailable, "stage guest secret environment failed", nil)
	}
	if stageErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secretEnvironmentCleanupTimeout)
		defer cancel()
		cleanupErr := service.cleanupGuestSecretEnvironment(cleanupCtx, snapshot, guestName)
		return stagedExecEnvironment{}, errors.Join(stageErr, cleanupErr)
	}
	staged.guest = guestName
	return staged, nil
}

func (service *LifecycleService) cleanupGuestSecretEnvironment(ctx context.Context, snapshot runtime.ResourceSnapshot, name runtime.GuestPath) error {
	if name == "" {
		return nil
	}
	spec := runtime.ExecSpec{
		Argv:       []string{DefaultGuestHelperPath, "exec", "--", "/bin/rm", "-rf", "--", string(name)},
		WorkingDir: workspaceGuestRoot,
		User:       service.user(),
	}
	exit, err := service.runtime.Exec(ctx, snapshot, spec, runtime.ExecIO{})
	if err != nil {
		return err
	}
	if exit.Code == nil || *exit.Code != 0 {
		return model.NewError(model.CodeUnavailable, "remove guest secret environment failed", nil)
	}
	return nil
}

func partitionExecEnvironment(environment map[string]string, secretKeys []string) (map[string]string, map[string]string, error) {
	secretNames := make(map[string]struct{}, len(secretKeys))
	for _, key := range secretKeys {
		if !validExecEnvironmentName(key) {
			return nil, nil, fmt.Errorf("invalid secret environment key %q", key)
		}
		secretNames[key] = struct{}{}
	}
	ordinary := make(map[string]string, len(environment))
	secret := make(map[string]string)
	for key, value := range environment {
		if !validExecEnvironmentName(key) || strings.IndexByte(value, 0) >= 0 {
			return nil, nil, fmt.Errorf("invalid environment entry %q", key)
		}
		if _, found := secretNames[key]; found {
			secret[key] = value
		} else {
			ordinary[key] = value
		}
	}
	return ordinary, secret, nil
}

func encodeSecretEnvironment(environment map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	contents := make([]byte, 0)
	for _, key := range keys {
		value := environment[key]
		if !validExecEnvironmentName(key) || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid environment entry %q", key)
		}
		if len(key)+1+len(value) > 4096 {
			return nil, fmt.Errorf("secret environment entry %q exceeds size limit", key)
		}
		if len(contents)+len(key)+1+len(value)+1 > maxStagedSecretEnvironmentBytes {
			return nil, errors.New("secret environment exceeds size limit")
		}
		contents = append(contents, key...)
		contents = append(contents, '=')
		contents = append(contents, value...)
		contents = append(contents, 0)
	}
	return contents, nil
}

func secretEnvironmentGuestPath(runID model.RunID) (runtime.GuestPath, error) {
	if _, err := model.ParseRunID(string(runID)); err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return runtime.GuestPath(path.Join("/tmp/dsx-run", string(runID), "env-"+hex.EncodeToString(random[:]))), nil
}

func validExecEnvironmentName(name string) bool {
	if name == "" || (name[0] != '_' && (name[0] < 'A' || name[0] > 'Z') && (name[0] < 'a' || name[0] > 'z')) {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if character != '_' && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
