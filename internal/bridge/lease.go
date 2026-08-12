package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/guest"
	"github.com/srimajji/dsx/internal/model"
)

const (
	MaxRelaySpecs       = 32
	MaxHelperInputBytes = 64 << 10
	MaxControlBytes     = 16 << 10
)

// LeaseIdentity is the complete owner of one workspace-scoped helper.
type LeaseIdentity struct {
	ProjectID     model.ProjectID     `json:"project_id"`
	CanonicalRoot string              `json:"canonical_root"`
	Workspace     model.WorkspaceName `json:"workspace"`
	RunID         model.RunID         `json:"run_id"`
}

func (identity LeaseIdentity) Validate() error {
	project, err := model.ParseProjectID(string(identity.ProjectID))
	if err != nil || project != identity.ProjectID {
		return errors.New("invalid bridge lease project identity")
	}
	derivedProject, err := model.NewProjectID(identity.CanonicalRoot)
	if err != nil || derivedProject != identity.ProjectID {
		return errors.New("bridge lease canonical root does not match project identity")
	}
	workspace, err := model.ParseWorkspaceName(string(identity.Workspace))
	if err != nil || workspace != identity.Workspace {
		return errors.New("invalid bridge lease workspace identity")
	}
	run, err := model.ParseRunID(string(identity.RunID))
	if err != nil || run != identity.RunID {
		return errors.New("invalid bridge lease run identity")
	}
	return nil
}

type RelayMode string

const (
	RelayModePrivateHost RelayMode = "private-host"
	RelayModePublication RelayMode = "publication"
)

// RelaySpec is an immutable, already-approved relay. Private-host relays use a
// dynamic listener port and return guest environment. Publications require an
// exact reserved listener and never return guest environment.
type RelaySpec struct {
	Name               string             `json:"name"`
	Mode               RelayMode          `json:"mode"`
	ListenerIP         netip.Addr         `json:"listener_ip"`
	ListenerPort       uint16             `json:"listener_port,omitempty"`
	OwnerIP            netip.Addr         `json:"owner_ip"`
	Destination        netip.AddrPort     `json:"destination"`
	DestinationLiteral bool               `json:"destination_literal"`
	AllowRemotePeers   bool               `json:"allow_remote_peers,omitempty"`
	Lease              time.Duration      `json:"lease"`
	Publication        *PublicationTarget `json:"publication,omitempty"`
}

// PublicationTarget pins the single owned workspace and guest helper used to
// reach a guest loopback listener without exposing the Apple runtime socket.
type PublicationTarget struct {
	WorkspaceID     string `json:"workspace_id"`
	GuestUser       string `json:"guest_user"`
	GuestHelperPath string `json:"guest_helper_path"`
}

// ListenerBinding is the complete non-secret listener truth returned by the
// helper. It deliberately excludes destination and control-token data.
type ListenerBinding struct {
	Name string     `json:"name"`
	Mode RelayMode  `json:"mode"`
	Addr netip.Addr `json:"address"`
	Port uint16     `json:"port"`
}

type LeaseResult struct {
	Bindings    []ListenerBinding `json:"bindings"`
	Environment map[string]string `json:"environment,omitempty"`
}

// LeaseStatus is deliberately destination- and token-free.
type LeaseStatus struct {
	State   string      `json:"state"`
	Failure string      `json:"failure,omitempty"`
	Result  LeaseResult `json:"result,omitempty"`
}

// LeaseManager owns helpers beyond the lifetime of the dsx command that
// created them. Ensure is idempotent only for the exact identity and spec set.
type LeaseManager interface {
	Ensure(context.Context, LeaseIdentity, []RelaySpec) (LeaseResult, error)
	Stop(context.Context, LeaseIdentity) error
	Status(context.Context, LeaseIdentity) (LeaseStatus, error)
}

func validateRelaySpecs(specs []RelaySpec) ([]RelaySpec, string, error) {
	if len(specs) == 0 || len(specs) > MaxRelaySpecs {
		return nil, "", fmt.Errorf("bridge lease must contain between 1 and %d relays", MaxRelaySpecs)
	}
	canonical := append([]RelaySpec(nil), specs...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Name < canonical[j].Name })
	for index, spec := range canonical {
		if _, err := relayEnvironmentBase(spec.Name); err != nil {
			return nil, "", err
		}
		if index != 0 && canonical[index-1].Name == spec.Name {
			return nil, "", fmt.Errorf("duplicate bridge relay name %q", spec.Name)
		}
		if !spec.ListenerIP.IsValid() || spec.ListenerIP.Zone() != "" || spec.ListenerIP.IsMulticast() {
			return nil, "", errors.New("bridge relay listener IP is invalid")
		}
		if !spec.OwnerIP.IsValid() || spec.OwnerIP.Zone() != "" || spec.OwnerIP.IsUnspecified() || spec.OwnerIP.IsMulticast() {
			return nil, "", errors.New("bridge relay owner IP is invalid")
		}
		if !spec.Destination.IsValid() || spec.Destination.Addr().Zone() != "" || spec.Destination.Port() == 0 || spec.Lease <= 0 || spec.Lease > MaxTCPLease {
			return nil, "", errors.New("bridge relay destination or lease is invalid")
		}
		switch spec.Mode {
		case RelayModePrivateHost:
			if spec.ListenerIP.IsUnspecified() || spec.ListenerPort != 0 || spec.AllowRemotePeers || spec.Publication != nil {
				return nil, "", errors.New("private host relay must use a specific dynamic listener without remote-peer exposure")
			}
		case RelayModePublication:
			loopback := netip.MustParseAddr("127.0.0.1")
			if spec.OwnerIP.Unmap() != loopback || spec.ListenerPort == 0 || !spec.DestinationLiteral {
				return nil, "", errors.New("publication must use an exact listener port, loopback owner policy, and literal private destination")
			}
			if spec.ListenerIP.IsLoopback() == spec.AllowRemotePeers {
				return nil, "", errors.New("publication remote-peer policy does not match its approved listener")
			}
			if err := validatePublicationTarget(spec.Publication); err != nil {
				return nil, "", err
			}
		default:
			return nil, "", fmt.Errorf("bridge relay %q has invalid mode %q", spec.Name, spec.Mode)
		}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("encode bridge relay plan: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func relayLeaseDuration(specs []RelaySpec) time.Duration {
	var duration time.Duration
	for _, spec := range specs {
		if spec.Lease > duration {
			duration = spec.Lease
		}
	}
	return duration
}

func validatePublicationTarget(target *PublicationTarget) error {
	if target == nil {
		return errors.New("publication guest execution target is required")
	}
	if len(target.WorkspaceID) == 0 || len(target.WorkspaceID) > 255 || strings.ContainsAny(target.WorkspaceID, "\x00/") {
		return errors.New("publication workspace identity is invalid")
	}
	if target.GuestHelperPath != guest.InstalledExecutable {
		return errors.New("publication guest helper path is not approved")
	}
	parts := strings.Split(target.GuestUser, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "0" || parts[1] == "0" {
		return errors.New("publication guest user must be a non-root numeric UID:GID")
	}
	for _, part := range parts {
		for _, character := range part {
			if character < '0' || character > '9' {
				return errors.New("publication guest user must be a non-root numeric UID:GID")
			}
		}
	}
	return nil
}

func relayEnvironmentBase(name string) (string, error) {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return "", fmt.Errorf("invalid bridge relay name %q", name)
	}
	var result strings.Builder
	result.Grow(len("DSX_BRIDGE_") + len(name))
	result.WriteString("DSX_BRIDGE_")
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
			result.WriteByte(byte(character - 'a' + 'A'))
		case character >= '0' && character <= '9', character == '_':
			result.WriteRune(character)
		case character == '-':
			result.WriteByte('_')
		default:
			return "", fmt.Errorf("invalid bridge relay name %q", name)
		}
	}
	return result.String(), nil
}
