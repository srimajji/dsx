package app

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

type hostBridgeResolver struct {
	answers [][]netip.Addr
	calls   int
}

func (resolver *hostBridgeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	index := resolver.calls
	resolver.calls++
	if index >= len(resolver.answers) {
		return nil, errors.New("unexpected resolution")
	}
	return append([]netip.Addr(nil), resolver.answers[index]...), nil
}

type hostBridgeRelay struct {
	address     netip.AddrPort
	destination netip.AddrPort
	done        chan struct{}
	closed      int
	err         error
}

func (relay *hostBridgeRelay) Addr() netip.AddrPort        { return relay.address }
func (relay *hostBridgeRelay) Destination() netip.AddrPort { return relay.destination }
func (relay *hostBridgeRelay) Done() <-chan struct{}       { return relay.done }
func (relay *hostBridgeRelay) Err() error                  { return relay.err }
func (relay *hostBridgeRelay) Close() error {
	relay.closed++
	select {
	case <-relay.done:
	default:
		close(relay.done)
	}
	return nil
}

type hostBridgeStarter struct {
	grants   []bridge.TCPGrant
	relays   []*hostBridgeRelay
	failAt   int
	nextPort uint16
}

func (starter *hostBridgeStarter) start(_ context.Context, grant bridge.TCPGrant) (hostTCPRelay, error) {
	starter.grants = append(starter.grants, grant)
	if starter.failAt > 0 && len(starter.grants) == starter.failAt {
		return nil, errors.New("injected relay start failure")
	}
	starter.nextPort++
	relay := &hostBridgeRelay{
		address: netip.AddrPortFrom(grant.ListenerIP, starter.nextPort), destination: grant.Destination, done: make(chan struct{}),
	}
	starter.relays = append(starter.relays, relay)
	return relay, nil
}

type hostBridgeLeaseManager struct {
	ensureIdentity bridge.LeaseIdentity
	ensureSpecs    []bridge.RelaySpec
	environment    map[string]string
	status         bridge.LeaseStatus
	stopIdentities []bridge.LeaseIdentity
	err            error
	onEnsure       func()
	onStop         func()
}

func (manager *hostBridgeLeaseManager) Ensure(_ context.Context, identity bridge.LeaseIdentity, specs []bridge.RelaySpec) (bridge.LeaseResult, error) {
	manager.ensureIdentity = identity
	manager.ensureSpecs = append([]bridge.RelaySpec(nil), specs...)
	if manager.onEnsure != nil {
		manager.onEnsure()
	}
	result := bridge.LeaseResult{Environment: cloneEnvironment(manager.environment)}
	for _, spec := range specs {
		port := spec.ListenerPort
		if port == 0 {
			base, _ := normalizedHostBridgeEnvironmentBase(spec.Name)
			parsed, _ := strconv.ParseUint(manager.environment[base+"_PORT"], 10, 16)
			port = uint16(parsed)
		}
		result.Bindings = append(result.Bindings, bridge.ListenerBinding{Name: spec.Name, Mode: spec.Mode, Addr: spec.ListenerIP, Port: port})
	}
	return result, manager.err
}

func (manager *hostBridgeLeaseManager) Stop(_ context.Context, identity bridge.LeaseIdentity) error {
	manager.stopIdentities = append(manager.stopIdentities, identity)
	if manager.onStop != nil {
		manager.onStop()
	}
	return manager.err
}

func (manager *hostBridgeLeaseManager) Status(context.Context, bridge.LeaseIdentity) (bridge.LeaseStatus, error) {
	return manager.status, manager.err
}

func hostBridgeService(resolver bridge.TCPResolver, starter *hostBridgeStarter, listener netip.Addr) *LifecycleService {
	return &LifecycleService{hostBridges: hostBridgeRuntime{
		resolver:    resolver,
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) { return listener, nil },
		startTCP:    starter.start,
		lease:       time.Hour,
	}}
}

func hostBridgeWorkspace(address string) runtime.ResourceSnapshot {
	owner := netip.MustParseAddr(address)
	return runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "workspace", Kind: runtime.ResourceWorkspace},
		State:    "running", Networks: []string{"owned"},
		NetworkAddresses: map[string][]netip.Addr{"owned": {owner}},
	}
}

func hostBridgePlan(grants ...plan.BridgeGrant) plan.ExecutionPlan {
	health := plan.ResolvedCommand{Argv: []string{"check"}, Env: []plan.EnvGrant{{Name: "HEALTH", Value: "1"}}}
	return plan.ExecutionPlan{
		Bridges:   grants,
		Setup:     []plan.ResolvedCommand{{Argv: []string{"setup"}, Env: []plan.EnvGrant{{Name: "SETUP", Value: "1"}}}},
		Processes: []plan.ResolvedProcess{{Name: "server", Command: plan.ResolvedCommand{Argv: []string{"serve"}, Env: []plan.EnvGrant{{Name: "PROCESS", Value: "1"}}}, Health: &plan.ResolvedHealth{Command: &health}}},
	}
}

func environmentGrantMap(grants []plan.EnvGrant) map[string]plan.EnvGrant {
	result := make(map[string]plan.EnvGrant, len(grants))
	for _, grant := range grants {
		result[grant.Name] = grant
	}
	return result
}

func TestHostBridgeOneGrantOneRelayAndExactNonsecretEnvironment(t *testing.T) {
	resolver := &hostBridgeResolver{answers: [][]netip.Addr{{netip.MustParseAddr("10.40.0.9")}, {netip.MustParseAddr("10.40.0.9")}}}
	starter := &hostBridgeStarter{nextPort: 41000}
	listener := netip.MustParseAddr("192.168.64.1")
	owner := netip.MustParseAddr("192.168.64.12")
	service := hostBridgeService(resolver, starter, listener)
	original := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "team-db", Destination: "db.internal", Port: 5432})

	updated, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace(owner.String()), original)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if len(starter.grants) != 1 || len(session.relays) != 1 {
		t.Fatalf("grant/relay cardinality = %d/%d", len(starter.grants), len(session.relays))
	}
	grant := starter.grants[0]
	if grant.OwnerIP != owner || grant.ListenerIP != listener || grant.Destination != netip.MustParseAddrPort("10.40.0.9:5432") || grant.Lease != time.Hour {
		t.Fatalf("relay grant = %#v", grant)
	}
	want := map[string]string{"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1", "DSX_BRIDGE_TEAM_DB_PORT": "41001"}
	if !reflect.DeepEqual(session.Environment(), want) {
		t.Fatalf("bridge environment = %#v", session.Environment())
	}
	harnessEnvironment, err := mergeHostBridgeEnvironment(map[string]string{"USER_SETTING": "kept"}, session.Environment())
	if err != nil {
		t.Fatal(err)
	}
	wantHarness := map[string]string{"USER_SETTING": "kept", "DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1", "DSX_BRIDGE_TEAM_DB_PORT": "41001"}
	if !reflect.DeepEqual(harnessEnvironment, wantHarness) {
		t.Fatalf("harness environment = %#v", harnessEnvironment)
	}
	commands := []plan.ResolvedCommand{updated.Setup[0], updated.Processes[0].Command, *updated.Processes[0].Health.Command}
	for _, command := range commands {
		grants := environmentGrantMap(command.Env)
		for key, value := range want {
			if got := grants[key]; got.Value != value || got.Secret || got.Reference != "" {
				t.Fatalf("%s grant = %#v", key, got)
			}
		}
	}
	if len(original.Setup[0].Env) != 1 || len(original.Processes[0].Command.Env) != 1 {
		t.Fatal("host bridge injection mutated the approved plan")
	}
	serialized := strings.ToLower(strings.Join([]string{want["DSX_BRIDGE_TEAM_DB_HOST"], want["DSX_BRIDGE_TEAM_DB_PORT"]}, " "))
	for _, forbidden := range []string{"tailscale", "control", "socket", "token", "secret"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("bridge environment contains forbidden %q data", forbidden)
		}
	}
}

func TestHostBridgeRejectsDuplicateNormalizedNamesAndMissingOwnerBeforeListener(t *testing.T) {
	for name, test := range map[string]struct {
		workspace runtime.ResourceSnapshot
		execution plan.ExecutionPlan
	}{
		"duplicate normalized name": {hostBridgeWorkspace("192.168.64.12"), hostBridgePlan(
			plan.BridgeGrant{Kind: "host", Name: "team-db", Destination: "10.40.0.9", Port: 5432},
			plan.BridgeGrant{Kind: "host", Name: "team_db", Destination: "10.40.0.10", Port: 5432},
		)},
		"missing inspected owner": {runtime.ResourceSnapshot{Networks: []string{"owned"}}, hostBridgePlan(
			plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432},
		)},
	} {
		t.Run(name, func(t *testing.T) {
			starter := &hostBridgeStarter{nextPort: 42000}
			service := hostBridgeService(nil, starter, netip.MustParseAddr("192.168.64.1"))
			if _, session, err := service.activateHostBridges(context.Background(), test.workspace, test.execution); err == nil || session != nil {
				t.Fatalf("activation = %#v, %v", session, err)
			}
			if len(starter.grants) != 0 {
				t.Fatalf("listener started before complete workspace/grant validation: %#v", starter.grants)
			}
		})
	}
}

func TestHostBridgeRejectsChangedDNSBeforeListener(t *testing.T) {
	resolver := &hostBridgeResolver{answers: [][]netip.Addr{{netip.MustParseAddr("10.40.0.9")}, {netip.MustParseAddr("10.40.0.10")}}}
	starter := &hostBridgeStarter{nextPort: 43000}
	service := hostBridgeService(resolver, starter, netip.MustParseAddr("192.168.64.1"))
	execution := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "db", Destination: "db.internal", Port: 5432})

	_, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), execution)
	if model.ErrorCodeOf(err) != model.CodeUnapproved || session != nil {
		t.Fatalf("changed DNS activation = %#v, %v", session, err)
	}
	if len(starter.grants) != 0 {
		t.Fatalf("changed DNS created a listener: %#v", starter.grants)
	}
}

func TestHostBridgeSeparatesSandboxOwnerIPs(t *testing.T) {
	owners := []string{"192.168.64.12", "192.168.64.13"}
	seen := make([]netip.Addr, 0, len(owners))
	for index, owner := range owners {
		starter := &hostBridgeStarter{nextPort: uint16(44000 + index*10)}
		service := hostBridgeService(nil, starter, netip.MustParseAddr("192.168.64.1"))
		execution := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432})
		_, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace(owner), execution)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, starter.grants[0].OwnerIP)
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if seen[0] == seen[1] || seen[0] != netip.MustParseAddr(owners[0]) || seen[1] != netip.MustParseAddr(owners[1]) {
		t.Fatalf("owner grants = %#v", seen)
	}
}

func TestHostBridgeFailureClosesEarlierRelaysAndNoGrantIsUnchanged(t *testing.T) {
	starter := &hostBridgeStarter{nextPort: 45000, failAt: 2}
	service := hostBridgeService(nil, starter, netip.MustParseAddr("192.168.64.1"))
	execution := hostBridgePlan(
		plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432},
		plan.BridgeGrant{Kind: "host", Name: "cache", Destination: "10.40.0.10", Port: 6379},
	)
	if _, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), execution); err == nil || session != nil {
		t.Fatalf("failed activation = %#v, %v", session, err)
	}
	if len(starter.relays) != 1 || starter.relays[0].closed != 1 {
		t.Fatalf("earlier relay closure = %#v", starter.relays)
	}

	noGrant := hostBridgePlan(plan.BridgeGrant{Kind: "internet", Name: "internet"})
	before := noGrant
	starter = &hostBridgeStarter{nextPort: 46000}
	service = hostBridgeService(nil, starter, netip.Addr{})
	updated, session, err := service.activateHostBridges(context.Background(), runtime.ResourceSnapshot{}, noGrant)
	if err != nil || session != nil || !reflect.DeepEqual(updated, before) || len(starter.grants) != 0 {
		t.Fatalf("no-grant behavior changed: session=%#v updated=%#v err=%v", session, updated, err)
	}
}

func TestHostBridgeEnvironmentCollisionAndPrematureClosureFailClosed(t *testing.T) {
	execution := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432})
	execution.Setup[0].Env = append(execution.Setup[0].Env, plan.EnvGrant{Name: "DSX_BRIDGE_DB_HOST", Value: "spoofed"})
	starter := &hostBridgeStarter{nextPort: 47000}
	service := hostBridgeService(nil, starter, netip.MustParseAddr("192.168.64.1"))
	if _, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), execution); model.ErrorCodeOf(err) != model.CodeInvalidInput || session != nil || len(starter.grants) != 0 {
		t.Fatalf("reserved collision activation = %#v calls=%d err=%v", session, len(starter.grants), err)
	}

	tailscaleStarter := &hostBridgeStarter{nextPort: 47500}
	service = hostBridgeService(nil, tailscaleStarter, netip.MustParseAddr("100.64.0.8"))
	clean := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432})
	if _, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), clean); err == nil || session != nil || len(tailscaleStarter.grants) != 0 {
		t.Fatalf("Tailscale route activation = %#v calls=%d err=%v", session, len(tailscaleStarter.grants), err)
	}

	closedStarter := &hostBridgeStarter{nextPort: 48000}
	service = hostBridgeService(nil, closedStarter, netip.MustParseAddr("192.168.64.1"))
	service.hostBridges.startTCP = func(_ context.Context, grant bridge.TCPGrant) (hostTCPRelay, error) {
		done := make(chan struct{})
		close(done)
		return &hostBridgeRelay{address: netip.AddrPortFrom(grant.ListenerIP, 48001), destination: grant.Destination, done: done, err: errors.New("closed")}, nil
	}
	clean = hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "db", Destination: "10.40.0.9", Port: 5432})
	if _, session, err := service.activateHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), clean); err == nil || session != nil {
		t.Fatalf("prematurely closed relay activation = %#v, %v", session, err)
	}
}

func TestPersistentHostBridgeUsesExactWorkspaceLeaseAndReturnsBeforeStop(t *testing.T) {
	resolver := &hostBridgeResolver{answers: [][]netip.Addr{{netip.MustParseAddr("10.40.0.9")}, {netip.MustParseAddr("10.40.0.9")}}}
	starter := &hostBridgeStarter{}
	service := hostBridgeService(resolver, starter, netip.MustParseAddr("192.168.64.1"))
	manager := &hostBridgeLeaseManager{environment: map[string]string{
		"DSX_BRIDGE_TEAM_DB_HOST": "192.168.64.1",
		"DSX_BRIDGE_TEAM_DB_PORT": "49152",
	}}
	service.bridgeLeases = manager
	projectID, _ := model.ParseProjectID("abcdefghijklmnopqrst")
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000099")
	identity := bridge.LeaseIdentity{ProjectID: projectID, Sandbox: "main", RunID: runID}
	execution := hostBridgePlan(plan.BridgeGrant{Kind: "host", Name: "team-db", Destination: "db.internal", Port: 5432})

	updated, session, _, err := service.ensurePersistentHostBridges(context.Background(), hostBridgeWorkspace("192.168.64.12"), execution, identity, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if manager.ensureIdentity != identity || len(manager.ensureSpecs) != 1 {
		t.Fatalf("lease ensure = %#v %#v", manager.ensureIdentity, manager.ensureSpecs)
	}
	spec := manager.ensureSpecs[0]
	if spec.Name != "team-db" || spec.ListenerIP != netip.MustParseAddr("192.168.64.1") || spec.OwnerIP != netip.MustParseAddr("192.168.64.12") || spec.Destination != netip.MustParseAddrPort("10.40.0.9:5432") {
		t.Fatalf("lease relay spec = %#v", spec)
	}
	if session == nil || len(session.relays) != 0 || len(manager.stopIdentities) != 0 {
		t.Fatalf("persistent session unexpectedly owns foreground relay: %#v stops=%#v", session, manager.stopIdentities)
	}
	if got := environmentGrantMap(updated.Processes[0].Command.Env)["DSX_BRIDGE_TEAM_DB_PORT"].Value; got != "49152" {
		t.Fatalf("persistent bridge environment port = %q", got)
	}
	if err := session.Close(); err != nil || len(manager.stopIdentities) != 0 {
		t.Fatalf("command return stopped persistent helper: %v %#v", err, manager.stopIdentities)
	}
	if err := service.stopPersistentHostBridges(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.stopIdentities, []bridge.LeaseIdentity{identity}) {
		t.Fatalf("exact stop identities = %#v", manager.stopIdentities)
	}
}
