package runtime

import (
	"errors"
	"fmt"
)

// VerifyWorkspacePostcondition compares the security-relevant grants visible in
// runtime inspection with the exact workspace specification that authorized
// creation or restart. Identity and ownership labels are verified separately.
func VerifyWorkspacePostcondition(observed ResourceSnapshot, expected WorkspaceSpec) error {
	if len(observed.Mounts) != len(expected.Mounts) {
		return fmt.Errorf("mount count is %d, want %d", len(observed.Mounts), len(expected.Mounts))
	}
	mounts := make(map[string]Mount, len(observed.Mounts))
	for _, mount := range observed.Mounts {
		if _, duplicate := mounts[mount.Target]; duplicate {
			return fmt.Errorf("duplicate observed mount target %q", mount.Target)
		}
		mounts[mount.Target] = mount
	}
	for _, mount := range expected.Mounts {
		actual, found := mounts[mount.Target]
		if !found || actual.Source != mount.Source || actual.Target != mount.Target ||
			actual.ReadOnly != mount.ReadOnly || actual.Type != mount.Type {
			return fmt.Errorf("mount target %q does not match", mount.Target)
		}
	}
	if !sameStringSet(observed.Networks, expected.Networks) {
		return errors.New("network attachments do not match")
	}
	if len(observed.Ports) != len(expected.Ports) {
		return fmt.Errorf("published port count is %d, want %d", len(observed.Ports), len(expected.Ports))
	}
	ports := make(map[string]struct{}, len(observed.Ports))
	for _, port := range observed.Ports {
		key := fmt.Sprintf("%s/%d/%d/%s", port.HostIP, port.HostPort, port.GuestPort, port.Protocol)
		if _, duplicate := ports[key]; duplicate {
			return fmt.Errorf("duplicate observed published port %q", key)
		}
		ports[key] = struct{}{}
	}
	for _, port := range expected.Ports {
		if port.HostPort == nil {
			return errors.New("expected workspace contains an unresolved dynamic host port")
		}
		key := fmt.Sprintf("%s/%d/%d/%s", port.HostIP, *port.HostPort, port.GuestPort, port.Protocol)
		if _, found := ports[key]; !found {
			return fmt.Errorf("published port %q does not match", key)
		}
	}
	return nil
}

func sameStringSet(observed, expected []string) bool {
	if len(observed) != len(expected) {
		return false
	}
	values := make(map[string]struct{}, len(observed))
	for _, value := range observed {
		if _, duplicate := values[value]; duplicate {
			return false
		}
		values[value] = struct{}{}
	}
	for _, value := range expected {
		if _, found := values[value]; !found {
			return false
		}
	}
	return true
}
