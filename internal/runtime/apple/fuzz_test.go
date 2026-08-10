package apple

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/srimajji/dsx/internal/runtime"
)

func FuzzRuntimeInspectJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`[{"configuration":{"id":"workspace-1","labels":{"dev.dsx.managed":"true","dev.dsx.future":"ambiguous"}},"status":{"state":"running"}}]`),
		[]byte(`[{"configuration":{"id":"workspace-1"}`),
		[]byte(`[{"configuration":{"id":"workspace-1","networks":[{"network":"private"},{"network":"private"}]}}]`),
		[]byte(`[{"configuration":{"id":"workspace-1","networks":[{"network":"one"},{"network":"two"}]},"status":{"networks":[{"network":"one","ipv4Address":"10.0.0.2/24"},{"network":"two","ipv4Address":"10.0.0.2/24"}]}}]`),
		[]byte(`[{"configuration":{"id":"workspace-1","networks":[{"network":"private"}]},"status":{"networks":[{"network":"private","ipv4Address":"::ffff:10.0.0.2/120","ipv6Address":"10.0.0.2/24"}]}}]`),
		{0xff, 0xfe, '[', ']'},
	} {
		f.Add(seed, byte(0))
	}
	for _, seed := range [][]byte{
		[]byte(`[{"id":"private","configuration":{"name":"private","labels":{"dev.dsx.managed":"true"}}}]`),
		[]byte(`[{"id":"private","configuration":{"name":"different"}}]`),
		[]byte(`[{"id":"private","configuration":{"name":"private","labels":{"dev.dsx.managed":"true","dev.dsx.managed":"true"}}}]`),
	} {
		f.Add(seed, byte(1))
	}

	f.Fuzz(func(t *testing.T, data []byte, decoder byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		before := bytes.Clone(data)
		if decoder%2 == 0 {
			snapshots, err := decodeContainers(data)
			if !bytes.Equal(data, before) {
				t.Fatal("decodeContainers mutated runtime output")
			}
			if err != nil {
				if snapshots != nil {
					t.Fatalf("decodeContainers returned partial snapshots on malformed output: %#v, %v", snapshots, err)
				}
				return
			}
			for _, snapshot := range snapshots {
				if snapshot.ID == "" || snapshot.Name == "" {
					t.Fatalf("decodeContainers accepted an empty identity: %#v", snapshot)
				}
				assertUnambiguousNetworkAddresses(t, snapshot.NetworkAddresses)
			}
			return
		}

		kind := runtime.ResourceNetwork
		if decoder&2 != 0 {
			kind = runtime.ResourceVolume
		}
		snapshots, err := decodeNamed(data, kind)
		if !bytes.Equal(data, before) {
			t.Fatal("decodeNamed mutated runtime output")
		}
		if err != nil {
			if snapshots != nil {
				t.Fatalf("decodeNamed returned partial snapshots on malformed output: %#v, %v", snapshots, err)
			}
			return
		}
		for _, snapshot := range snapshots {
			if snapshot.ID == "" || snapshot.Name != string(snapshot.ID) || snapshot.Kind != kind {
				t.Fatalf("decodeNamed accepted an ambiguous identity: %#v", snapshot)
			}
		}
	})
}

func FuzzRuntimeAmbiguousIP(f *testing.F) {
	for _, address := range []string{"10.0.0.2/24", "2001:db8::2/64", "::ffff:10.0.0.2/120", "not-an-address", "\x1b[2J"} {
		f.Add(address)
	}
	f.Fuzz(func(t *testing.T, address string) {
		if len(address) > 256 {
			t.Skip()
		}
		if address == "" {
			return
		}
		encodedAddress, err := json.Marshal(address)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(`[{"configuration":{"id":"workspace-1","networks":[{"network":"one"},{"network":"two"}]},"status":{"networks":[{"network":"one","ipv4Address":` + string(encodedAddress) + `},{"network":"two","ipv4Address":` + string(encodedAddress) + `}]}}]`)
		if _, err := decodeContainers(payload); err == nil {
			t.Fatalf("duplicate or invalid address %q was accepted for two networks", address)
		}
	})
}

func assertUnambiguousNetworkAddresses(t *testing.T, networks map[string][]netip.Addr) {
	t.Helper()
	seen := make(map[netip.Addr]string)
	for network, addresses := range networks {
		for _, address := range addresses {
			if !address.IsValid() || address.IsUnspecified() {
				t.Fatalf("network %q contains invalid address %q", network, address)
			}
			if previous, ok := seen[address]; ok {
				t.Fatalf("address %q appears on networks %q and %q", address, previous, network)
			}
			seen[address] = network
		}
	}
}
