package apple

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

func TestProbeInstalledSmoke(t *testing.T) {
	executable := os.Getenv("DSX_APPLE_CONTAINER")
	if executable == "" {
		t.Skip("set DSX_APPLE_CONTAINER to the absolute container 1.2.2 executable for the read-only Apple smoke")
	}
	ctx := context.Background()
	runner := OSRunner{}
	before := installedIdentitySnapshot(t, ctx, runner, executable)

	adapter, err := NewAdapter(runner, executable)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	versionResult := runInstalledReadOnly(t, ctx, runner, executable, "system", "version", "--format", "json")
	cliVersion, serverVersion, err := decodeSystemVersion(versionResult.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	statusResult := runInstalledReadOnly(t, ctx, runner, executable, "system", "status", "--format", "json")
	status, statusServerVersion, err := decodeSystemStatus(statusResult.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.CLIVersion != cliVersion || capabilities.ServerVersion != serverVersion || statusServerVersion != serverVersion {
		t.Fatalf("probe versions (%q, %q) do not match system JSON (%q, %q, status %q)", capabilities.CLIVersion, capabilities.ServerVersion, cliVersion, serverVersion, statusServerVersion)
	}
	if capabilities.ServiceHealthy != (status == "running") {
		t.Fatalf("probe service health %t does not match JSON status %q", capabilities.ServiceHealthy, status)
	}

	after := installedIdentitySnapshot(t, ctx, runner, executable)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only probe changed runtime identity sets: before=%#v after=%#v", before, after)
	}
	t.Logf("host=%s %s/%s cli=%s server=%s status=%s builderHealthy=%t identities=%#v", capabilities.HostOS, capabilities.HostVersion, capabilities.HostArch, capabilities.CLIVersion, capabilities.ServerVersion, status, capabilities.BuilderHealthy, after)
}

type installedIdentities struct {
	Containers []string
	Networks   []string
	Volumes    []string
}

func installedIdentitySnapshot(t *testing.T, ctx context.Context, runner Runner, executable string) installedIdentities {
	t.Helper()
	containersResult := runInstalledReadOnly(t, ctx, runner, executable, "list", "--all", "--format", "json")
	var containers []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(containersResult.Stdout, &containers); err != nil {
		t.Fatal(err)
	}

	networksResult := runInstalledReadOnly(t, ctx, runner, executable, "network", "list", "--format", "json")
	var networks []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(networksResult.Stdout, &networks); err != nil {
		t.Fatal(err)
	}

	volumesResult := runInstalledReadOnly(t, ctx, runner, executable, "volume", "list", "--format", "json")
	var volumes []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(volumesResult.Stdout, &volumes); err != nil {
		t.Fatal(err)
	}

	identities := installedIdentities{
		Containers: make([]string, 0, len(containers)),
		Networks:   make([]string, 0, len(networks)),
		Volumes:    make([]string, 0, len(volumes)),
	}
	for _, container := range containers {
		if container.Configuration.ID == "" {
			t.Fatal("container list JSON contains an empty configuration ID")
		}
		identities.Containers = append(identities.Containers, container.Configuration.ID)
	}
	for _, network := range networks {
		if network.ID == "" {
			t.Fatal("network list JSON contains an empty ID")
		}
		identities.Networks = append(identities.Networks, network.ID)
	}
	for _, volume := range volumes {
		if volume.ID == "" {
			t.Fatal("volume list JSON contains an empty ID")
		}
		identities.Volumes = append(identities.Volumes, volume.ID)
	}
	sort.Strings(identities.Containers)
	sort.Strings(identities.Networks)
	sort.Strings(identities.Volumes)
	return identities
}

func runInstalledReadOnly(t *testing.T, ctx context.Context, runner Runner, executable string, args ...string) Result {
	t.Helper()
	result, err := runner.Run(ctx, Command{
		Executable: executable,
		Args:       append([]string(nil), args...),
		Env:        append([]string(nil), probeEnvironment...),
	})
	if err != nil {
		t.Fatalf("read-only command %q failed: %v (stderr %q)", args, err, result.Stderr)
	}
	return result
}
