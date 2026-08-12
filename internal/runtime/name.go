package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/srimajji/dsx/internal/model"
)

const (
	MaxGeneratedResourceNameBytes = 62
	MaxResourceRoleBytes          = 9
	maxProjectComponentBytes      = 16
)

const (
	AuthLoginManagedLabel  = "dev.dsx.managed"
	AuthLoginContractLabel = "dev.dsx.contract"
	AuthLoginProjectLabel  = "dev.dsx.project"
	AuthLoginRunLabel      = "dev.dsx.run"
	AuthLoginKindLabel     = "dev.dsx.kind"
	AuthLoginHarnessLabel  = "dev.dsx.harness"
	AuthLoginContractValue = "dsx.auth-login/v1"
)

// CanonicalResourceName returns the readable runtime name for a resource in a
// current named workspace. The full project ID remains in ownership labels;
// the display component and short path hash are never ownership authority.
func CanonicalResourceName(canonicalRoot string, workspace model.WorkspaceName, role string) (string, error) {
	if canonicalRoot == "" || !filepath.IsAbs(canonicalRoot) || filepath.Clean(canonicalRoot) != canonicalRoot {
		return "", errors.New("canonical project root must be a clean absolute path")
	}
	parsedWorkspace, err := model.ParseWorkspaceName(string(workspace))
	if err != nil || parsedWorkspace != workspace {
		return "", fmt.Errorf("runtime workspace name: %w", err)
	}
	parsedRole, err := model.ParseWorkspaceName(role)
	if err != nil || string(parsedRole) != role || len(role) > MaxResourceRoleBytes {
		return "", fmt.Errorf("runtime resource role %q is invalid", role)
	}

	project := sanitizeProjectComponent(filepath.Base(canonicalRoot))
	digest := sha256.Sum256([]byte(canonicalRoot))
	hash := hex.EncodeToString(digest[:3])
	name := "dsx-" + project + "-" + string(workspace) + "-" + role + "-" + hash
	if len(name) > MaxGeneratedResourceNameBytes {
		return "", fmt.Errorf("runtime resource name exceeds %d bytes", MaxGeneratedResourceNameBytes)
	}
	return name, nil
}

// CanonicalAuthLoginName returns the stable project-and-harness name used by a
// serialized disposable authentication-login session.
func CanonicalAuthLoginName(canonicalRoot, harness string) (string, error) {
	if canonicalRoot == "" || !filepath.IsAbs(canonicalRoot) || filepath.Clean(canonicalRoot) != canonicalRoot {
		return "", errors.New("canonical project root must be a clean absolute path")
	}
	if !validNameToken(harness, MaxResourceRoleBytes) {
		return "", fmt.Errorf("auth login harness %q is invalid", harness)
	}
	project := sanitizeProjectComponent(filepath.Base(canonicalRoot))
	digest := sha256.Sum256([]byte(canonicalRoot))
	hash := hex.EncodeToString(digest[:3])
	name := "dsx-" + project + "-auth-" + harness + "-" + hash
	if len(name) > MaxGeneratedResourceNameBytes {
		return "", fmt.Errorf("auth login resource name exceeds %d bytes", MaxGeneratedResourceNameBytes)
	}
	return name, nil
}

func AuthLoginOwnershipLabels(projectID model.ProjectID, runID model.RunID, harness string) ([]Label, error) {
	if _, err := model.ParseProjectID(string(projectID)); err != nil {
		return nil, err
	}
	if _, err := model.ParseRunID(string(runID)); err != nil {
		return nil, err
	}
	if !validNameToken(harness, MaxResourceRoleBytes) {
		return nil, fmt.Errorf("auth login harness %q is invalid", harness)
	}
	return []Label{
		{Key: AuthLoginManagedLabel, Value: "true"},
		{Key: AuthLoginContractLabel, Value: AuthLoginContractValue},
		{Key: AuthLoginProjectLabel, Value: string(projectID)},
		{Key: AuthLoginRunLabel, Value: string(runID)},
		{Key: AuthLoginKindLabel, Value: string(ResourceAuthLogin)},
		{Key: AuthLoginHarnessLabel, Value: harness},
	}, nil
}

func validNameToken(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func sanitizeProjectComponent(folder string) string {
	var component strings.Builder
	component.Grow(min(len(folder), maxProjectComponentBytes))
	pendingSeparator := false
	for index := 0; index < len(folder) && component.Len() < maxProjectComponentBytes; index++ {
		value := folder[index]
		if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		if (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			pendingSeparator = component.Len() > 0
			continue
		}
		if pendingSeparator {
			component.WriteByte('-')
			if component.Len() == maxProjectComponentBytes {
				break
			}
			pendingSeparator = false
		}
		component.WriteByte(value)
	}
	value := strings.Trim(component.String(), "-")
	if value == "" {
		return "project"
	}
	return value
}
