package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	hostAWSWorkspaceDirectoryName = "host-aws-workspaces"
	hostAWSWorkspaceDataName      = "publication"
	leappMirrorCurrentName        = "current"
	leappMirrorGenerationPrefix   = ".generation-"
	leappMirrorWritePrefix        = ".host-aws-publication-"
	leappMirrorCurrentPrefix      = ".current-"
	leappMirrorLedgerName         = "helper.json"
	leappMirrorTokenName          = "helper-control.token"
	leappMirrorFailureName        = "helper-failure.json"
	leappMirrorSocketName         = "helper-control.sock"
	leappMirrorLockName           = ".host-aws-workspace-lock"
	leappMirrorPollInterval       = 100 * time.Millisecond
)

// HostAWSMirrorStatus contains only non-secret publication and helper health.
type HostAWSMirrorStatus struct {
	State   string `json:"state"`
	Failure string `json:"failure,omitempty"`
}

// HostAWSWorkspaceManager owns a private, stable publication channel for each
// workspace. Helper lifetime never changes the returned channel path.
type HostAWSWorkspaceManager interface {
	Prepare(context.Context, LeaseIdentity) (string, error)
	Enable(context.Context, LeaseIdentity, HostAWSAuthority) (string, error)
	Disable(context.Context, LeaseIdentity) error
	Remove(context.Context, LeaseIdentity) error
	Status(context.Context, LeaseIdentity) (HostAWSMirrorStatus, error)
}

type leappMirrorSpec struct {
	CanonicalPath string                `json:"canonical_path"`
	Source        HostAWSSourceIdentity `json:"source"`
}

func validatedLeappMirrorSpec(authority HostAWSAuthority) (leappMirrorSpec, string, error) {
	if authority.CanonicalPath == "" || !filepath.IsAbs(authority.CanonicalPath) || filepath.Clean(authority.CanonicalPath) != authority.CanonicalPath || authority.Identity == "" {
		return leappMirrorSpec{}, "", errors.New("host AWS mirror authority is incomplete")
	}
	var source HostAWSSourceIdentity
	count, err := fmt.Sscanf(authority.Identity, "dev=%d;ino=%d;uid=%d", &source.Device, &source.Inode, &source.UID)
	if err != nil || count != 3 || authority.Identity != fmt.Sprintf("dev=%d;ino=%d;uid=%d", source.Device, source.Inode, source.UID) || source.Device == 0 || source.Inode == 0 || source.UID != uint32(os.Geteuid()) {
		return leappMirrorSpec{}, "", errors.New("host AWS mirror source identity is invalid")
	}
	spec := leappMirrorSpec{CanonicalPath: authority.CanonicalPath, Source: source}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return leappMirrorSpec{}, "", err
	}
	digest := sha256.Sum256(encoded)
	return spec, hex.EncodeToString(digest[:]), nil
}

func leappAuthorityForSpec(spec leappMirrorSpec) HostAWSAuthority {
	return HostAWSAuthority{
		DeclaredPath:  spec.CanonicalPath,
		CanonicalPath: spec.CanonicalPath,
		Identity:      fmt.Sprintf("dev=%d;ino=%d;uid=%d", spec.Source.Device, spec.Source.Inode, spec.Source.UID),
	}
}
