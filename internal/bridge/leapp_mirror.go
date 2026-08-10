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
	leappMirrorDirectoryName    = "leapp-mirrors"
	leappMirrorDataName         = "mirror"
	leappMirrorCurrentName      = "current"
	leappMirrorGenerationPrefix = ".generation-"
	leappMirrorWritePrefix      = ".leapp-mirror-"
	leappMirrorCurrentPrefix    = ".current-"
	leappMirrorLedgerName       = "mirror.json"
	leappMirrorTokenName        = "mirror-control.token"
	leappMirrorFailureName      = "mirror-failure.json"
	leappMirrorSocketName       = "mirror-control.sock"
	leappMirrorLockName         = ".mirror-lock"
	leappMirrorPollInterval     = 100 * time.Millisecond
)

// LeappMirrorStatus contains only non-secret helper health.
type LeappMirrorStatus struct {
	State   string `json:"state"`
	Failure string `json:"failure,omitempty"`
}

// LeappMirrorManager owns the temporary helper that maintains one stable,
// private path for a workspace's Leapp credentials.
type LeappMirrorManager interface {
	Ensure(context.Context, LeaseIdentity, LeappAuthority) (string, error)
	Path(LeaseIdentity) (string, error)
	Stop(context.Context, LeaseIdentity) error
	Status(context.Context, LeaseIdentity) (LeappMirrorStatus, error)
}

type leappMirrorSpec struct {
	CanonicalPath string              `json:"canonical_path"`
	Source        LeappSourceIdentity `json:"source"`
}

func validatedLeappMirrorSpec(authority LeappAuthority) (leappMirrorSpec, string, error) {
	if authority.CanonicalPath == "" || !filepath.IsAbs(authority.CanonicalPath) || filepath.Clean(authority.CanonicalPath) != authority.CanonicalPath || authority.Identity == "" {
		return leappMirrorSpec{}, "", errors.New("Leapp mirror authority is incomplete")
	}
	var source LeappSourceIdentity
	count, err := fmt.Sscanf(authority.Identity, "dev=%d;ino=%d;uid=%d", &source.Device, &source.Inode, &source.UID)
	if err != nil || count != 3 || authority.Identity != fmt.Sprintf("dev=%d;ino=%d;uid=%d", source.Device, source.Inode, source.UID) || source.Device == 0 || source.Inode == 0 || source.UID != uint32(os.Geteuid()) {
		return leappMirrorSpec{}, "", errors.New("Leapp mirror source identity is invalid")
	}
	spec := leappMirrorSpec{CanonicalPath: authority.CanonicalPath, Source: source}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return leappMirrorSpec{}, "", err
	}
	digest := sha256.Sum256(encoded)
	return spec, hex.EncodeToString(digest[:]), nil
}

func leappAuthorityForSpec(spec leappMirrorSpec) LeappAuthority {
	return LeappAuthority{
		DeclaredPath:  spec.CanonicalPath,
		CanonicalPath: spec.CanonicalPath,
		Identity:      fmt.Sprintf("dev=%d;ino=%d;uid=%d", spec.Source.Device, spec.Source.Inode, spec.Source.UID),
	}
}
