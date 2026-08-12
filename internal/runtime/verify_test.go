package runtime

import (
	"net/netip"
	"testing"
)

func TestVerifyWorkspacePostconditionAcceptsResolvedDynamicPort(t *testing.T) {
	expected := WorkspaceSpec{
		Mounts: []Mount{{Source: "workspace", Target: "/workspace", Type: "volume", Authority: MountAuthorityVolume}},
		Networks: []string{"private-network"},
		Ports: []PortRequest{{
			HostIP: netip.MustParseAddr("127.0.0.1"), GuestPort: 3000, Protocol: "tcp",
		}},
	}
	observed := ResourceSnapshot{
		Mounts: []Mount{{Source: "workspace", Target: "/workspace", Type: "volume"}},
		Networks: []string{"private-network"},
		Ports: []PortBinding{{
			HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: 49152, GuestPort: 3000, Protocol: "tcp",
		}},
	}
	if err := VerifyWorkspacePostcondition(observed, expected); err != nil {
		t.Fatal(err)
	}
	observed.Ports[0].HostIP = netip.MustParseAddr("0.0.0.0")
	if err := VerifyWorkspacePostcondition(observed, expected); err == nil {
		t.Fatal("accepted a non-loopback dynamic port result")
	}
}

func TestVerifyWorkspacePostconditionRejectsUnexpectedHostMount(t *testing.T) {
	expected := WorkspaceSpec{
		Mounts: []Mount{{Source: "workspace", Target: "/workspace", Type: "volume", Authority: MountAuthorityVolume}},
	}
	observed := ResourceSnapshot{
		Mounts: []Mount{{Source: "/Users/developer/project", Target: "/workspace", Type: "bind"}},
	}
	if err := VerifyWorkspacePostcondition(observed, expected); err == nil {
		t.Fatal("accepted host source mount in runtime postcondition")
	}
}
