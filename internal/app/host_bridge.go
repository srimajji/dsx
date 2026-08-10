package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/ports"
	"github.com/srimajji/dsx/internal/runtime"
)

const hostBridgeEnvironmentPrefix = "DSX_BRIDGE_"

var hostBridgeSharedIPv4 = netip.MustParsePrefix("100.64.0.0/10")

type hostTCPRelay interface {
	Addr() netip.AddrPort
	Destination() netip.AddrPort
	Done() <-chan struct{}
	Err() error
	Close() error
}

type hostBridgeRuntime struct {
	resolver    bridge.TCPResolver
	routeSource func(context.Context, netip.Addr) (netip.Addr, error)
	startTCP    func(context.Context, bridge.TCPGrant) (hostTCPRelay, error)
	lease       time.Duration
}

type hostBridgeSession struct {
	relays      []hostTCPRelay
	environment map[string]string
}

type preparedHostBridge struct {
	grant       plan.BridgeGrant
	base        string
	destination netip.AddrPort
}

func defaultHostBridgeRuntime() hostBridgeRuntime {
	return hostBridgeRuntime{
		routeSource: bridge.RouteSourceAddr,
		startTCP: func(ctx context.Context, grant bridge.TCPGrant) (hostTCPRelay, error) {
			return bridge.StartTCP(ctx, grant)
		},
		lease: bridge.MaxTCPLease,
	}
}

// activateHostBridges owns only foreground-scoped relay handles. Relays are not
// Apple resources and are deliberately absent from the durable resource graph:
// normal cleanup calls Close, cancellation expires their contexts, and a crash
// releases every in-process listener when the dsx process exits.
func (service *LifecycleService) activateHostBridges(ctx context.Context, workspace runtime.ResourceSnapshot, execution plan.ExecutionPlan) (plan.ExecutionPlan, *hostBridgeSession, error) {
	prepared, err := prepareHostBridgeGrants(ctx, service.hostBridges, workspace, execution)
	if err != nil {
		return execution, nil, err
	}
	if len(prepared) == 0 {
		return execution, nil, nil
	}
	owner, err := workspaceOwnerAddress(workspace)
	if err != nil {
		return execution, nil, err
	}
	listener, err := service.hostBridges.routeSource(ctx, owner)
	if err != nil {
		return execution, nil, model.Wrap(model.CodeUnavailable, "derive host bridge route address", err)
	}
	listener = listener.Unmap()
	if !listener.IsValid() || listener.IsUnspecified() || listener.IsLoopback() || listener.IsMulticast() || listener.IsLinkLocalUnicast() || (listener.Is4() && hostBridgeSharedIPv4.Contains(listener)) {
		return execution, nil, model.NewError(model.CodeUnavailable, "host bridge route address is unsupported", nil)
	}

	session := &hostBridgeSession{relays: make([]hostTCPRelay, 0, len(prepared)), environment: make(map[string]string, len(prepared)*2)}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = session.Close()
		}
	}()
	for _, item := range prepared {
		if err := bridge.RevalidateTCPDestination(ctx, service.hostBridges.resolver, item.grant.Destination, item.destination); err != nil {
			return execution, nil, model.Wrap(model.CodeUnapproved, "host bridge destination changed before relay readiness", err)
		}
		_, literalErr := netip.ParseAddr(item.grant.Destination)
		relay, err := service.hostBridges.startTCP(ctx, bridge.TCPGrant{
			ListenerIP:         listener,
			OwnerIP:            owner,
			Destination:        item.destination,
			DestinationLiteral: literalErr == nil,
			Lease:              service.hostBridges.lease,
		})
		if err != nil {
			return execution, nil, model.Wrap(model.CodeUnavailable, "start approved host bridge relay", err)
		}
		if err := validateReadyHostRelay(relay, listener, item.destination); err != nil {
			_ = relay.Close()
			return execution, nil, err
		}
		session.relays = append(session.relays, relay)
		address := relay.Addr()
		session.environment[item.base+"_HOST"] = address.Addr().String()
		session.environment[item.base+"_PORT"] = strconv.Itoa(int(address.Port()))
	}
	updated, err := applyHostBridgePlanEnvironment(execution, session.environment)
	if err != nil {
		return execution, nil, err
	}
	closeOnError = false
	return updated, session, nil
}

func (service *LifecycleService) ensurePersistentHostBridges(ctx context.Context, workspace runtime.ResourceSnapshot, execution plan.ExecutionPlan, identity bridge.LeaseIdentity, publications []ports.PublishedBinding, includePrivate bool) (plan.ExecutionPlan, *hostBridgeSession, bridge.LeaseResult, error) {
	prepared, err := prepareHostBridgeGrants(ctx, service.hostBridges, workspace, execution)
	if err != nil {
		return execution, nil, bridge.LeaseResult{}, err
	}
	if !includePrivate {
		prepared = nil
	}
	if len(prepared) == 0 && len(publications) == 0 {
		return execution, nil, bridge.LeaseResult{}, nil
	}
	if service.bridgeLeases == nil {
		return execution, nil, bridge.LeaseResult{}, model.NewError(model.CodeUnavailable, "persistent host relay lease manager is unavailable", nil)
	}
	if err := identity.Validate(); err != nil {
		return execution, nil, bridge.LeaseResult{}, model.NewError(model.CodeInvalidInput, "invalid persistent host relay identity", err)
	}
	workspaceIP, err := workspaceOwnerAddress(workspace)
	if err != nil {
		return execution, nil, bridge.LeaseResult{}, err
	}
	specs := make([]bridge.RelaySpec, 0, len(prepared)+len(publications))
	if len(prepared) != 0 {
		listener, routeErr := service.hostBridges.routeSource(ctx, workspaceIP)
		if routeErr != nil {
			return execution, nil, bridge.LeaseResult{}, model.Wrap(model.CodeUnavailable, "derive host bridge route address", routeErr)
		}
		listener = listener.Unmap()
		if !listener.IsValid() || listener.IsUnspecified() || listener.IsLoopback() || listener.IsMulticast() || listener.IsLinkLocalUnicast() || (listener.Is4() && hostBridgeSharedIPv4.Contains(listener)) {
			return execution, nil, bridge.LeaseResult{}, model.NewError(model.CodeUnavailable, "host bridge route address is unsupported", nil)
		}
		for _, item := range prepared {
			if err := bridge.RevalidateTCPDestination(ctx, service.hostBridges.resolver, item.grant.Destination, item.destination); err != nil {
				return execution, nil, bridge.LeaseResult{}, model.Wrap(model.CodeUnapproved, "host bridge destination changed before relay readiness", err)
			}
			_, literalErr := netip.ParseAddr(item.grant.Destination)
			specs = append(specs, bridge.RelaySpec{
				Name: item.grant.Name, Mode: bridge.RelayModePrivateHost, ListenerIP: listener, OwnerIP: workspaceIP,
				Destination: item.destination, DestinationLiteral: literalErr == nil, Lease: service.hostBridges.lease,
			})
		}
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, publication := range publications {
		specs = append(specs, bridge.RelaySpec{
			Name: publication.Name, Mode: bridge.RelayModePublication,
			ListenerIP: publication.HostIP, ListenerPort: publication.HostPort, OwnerIP: loopback,
			Destination: netip.AddrPortFrom(workspaceIP, publication.GuestPort), DestinationLiteral: true,
			AllowRemotePeers: !publication.HostIP.IsLoopback(), Lease: service.hostBridges.lease,
			Publication: &bridge.PublicationTarget{
				WorkspaceID: string(workspace.ID), GuestUser: service.user(), GuestHelperPath: DefaultGuestHelperPath,
			},
		})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	result, err := service.bridgeLeases.Ensure(ctx, identity, specs)
	if err != nil {
		return execution, nil, bridge.LeaseResult{}, model.Wrap(model.CodeUnavailable, "ensure persistent host relay lease", err)
	}
	if !validPersistentRelayResult(specs, result) {
		return execution, nil, bridge.LeaseResult{}, model.NewError(model.CodeAmbiguous, "persistent host relay readiness differs from the approved listeners", nil)
	}
	updated, err := applyHostBridgePlanEnvironment(execution, result.Environment)
	if err != nil {
		return execution, nil, bridge.LeaseResult{}, err
	}
	return updated, &hostBridgeSession{environment: cloneEnvironment(result.Environment)}, result, nil
}

func validPersistentRelayResult(specs []bridge.RelaySpec, result bridge.LeaseResult) bool {
	if len(result.Bindings) != len(specs) {
		return false
	}
	for index, spec := range specs {
		binding := result.Bindings[index]
		if binding.Name != spec.Name || binding.Mode != spec.Mode || binding.Addr.Unmap() != spec.ListenerIP.Unmap() || binding.Port == 0 || (spec.ListenerPort != 0 && binding.Port != spec.ListenerPort) {
			return false
		}
	}
	return true
}

func validatePersistentHostBridgeEnvironment(prepared []preparedHostBridge, listener netip.Addr, environment map[string]string) error {
	if len(environment) != len(prepared)*2 {
		return model.NewError(model.CodeAmbiguous, "persistent host bridge returned an incomplete environment", nil)
	}
	for _, item := range prepared {
		host, hostFound := environment[item.base+"_HOST"]
		port, portFound := environment[item.base+"_PORT"]
		address, addressErr := netip.ParseAddr(host)
		parsedPort, portErr := strconv.ParseUint(port, 10, 16)
		if !hostFound || !portFound || addressErr != nil || address.Unmap() != listener.Unmap() || portErr != nil || parsedPort == 0 {
			return model.NewError(model.CodeAmbiguous, "persistent host bridge readiness differs from the approved listener", nil)
		}
	}
	return nil
}

func (service *LifecycleService) stopPersistentHostBridges(ctx context.Context, identity bridge.LeaseIdentity) error {
	if service == nil || service.bridgeLeases == nil {
		return nil
	}
	if err := service.bridgeLeases.Stop(ctx, identity); err != nil {
		return model.Wrap(model.CodeUnavailable, "stop persistent host bridge lease", err)
	}
	return nil
}

func persistentHostBridgeWarning(status bridge.LeaseStatus, err error) string {
	if err != nil {
		return "host bridge lease ownership is ambiguous"
	}
	switch status.State {
	case "running":
		return ""
	case "error":
		switch status.Failure {
		case "expired", "relay_failed", "control_failed", "startup_failed":
			return "host bridge lease is unavailable (" + status.Failure + ")"
		default:
			return "host bridge lease is unavailable"
		}
	case "dead":
		return "host bridge lease helper is not reachable"
	default:
		return "host bridge lease is absent"
	}
}
func (service *LifecycleService) persistentHostBridgeWarnings(ctx context.Context, execution plan.ExecutionPlan, identity bridge.LeaseIdentity, hasPublications bool) []string {
	if !hasHostBridgeGrants(execution) && !hasPublications {
		return nil
	}
	if service == nil || service.bridgeLeases == nil {
		return []string{"host bridge lease status is unavailable"}
	}
	status, err := service.bridgeLeases.Status(ctx, identity)
	warning := persistentHostBridgeWarning(status, err)
	if warning == "" {
		return nil
	}
	return []string{warning}
}

func prepareHostBridgeGrants(ctx context.Context, hostRuntime hostBridgeRuntime, workspace runtime.ResourceSnapshot, execution plan.ExecutionPlan) ([]preparedHostBridge, error) {
	prepared := make([]preparedHostBridge, 0, len(execution.Bridges))
	bases := make(map[string]string)
	for _, grant := range execution.Bridges {
		if grant.Kind != "host" {
			continue
		}
		base, err := normalizedHostBridgeEnvironmentBase(grant.Name)
		if err != nil {
			return nil, model.NewError(model.CodeInvalidInput, err.Error(), nil)
		}
		if first, duplicate := bases[base]; duplicate {
			return nil, model.NewError(model.CodeInvalidInput, fmt.Sprintf("host bridge names %q and %q normalize to the same environment name", first, grant.Name), nil)
		}
		bases[base] = grant.Name
		prepared = append(prepared, preparedHostBridge{grant: grant, base: base})
	}
	if len(prepared) == 0 {
		return nil, nil
	}
	if ctx == nil {
		return nil, model.NewError(model.CodeInvalidInput, "host bridge context is nil", nil)
	}
	if hostRuntime.routeSource == nil || hostRuntime.startTCP == nil || hostRuntime.lease <= 0 || hostRuntime.lease > bridge.MaxTCPLease {
		return nil, model.NewError(model.CodeUnavailable, "host bridge runtime is unavailable", nil)
	}
	if _, err := workspaceOwnerAddress(workspace); err != nil {
		return nil, err
	}
	if err := validateHostBridgePlanEnvironmentSlots(execution, bases); err != nil {
		return nil, err
	}
	for index := range prepared {
		destination, err := bridge.ResolveTCPDestination(ctx, hostRuntime.resolver, prepared[index].grant.Destination, int(prepared[index].grant.Port))
		if err != nil {
			return nil, model.Wrap(model.CodeUnavailable, "resolve approved host bridge destination", err)
		}
		prepared[index].destination = destination
	}
	return prepared, nil
}

func workspaceOwnerAddress(workspace runtime.ResourceSnapshot) (netip.Addr, error) {
	if len(workspace.Networks) != 1 {
		return netip.Addr{}, model.NewError(model.CodeAmbiguous, "workspace must have exactly one inspected network for host bridge ownership", nil)
	}
	addresses, found := workspace.NetworkAddresses[workspace.Networks[0]]
	if !found {
		return netip.Addr{}, model.NewError(model.CodeAmbiguous, "workspace inspected owner-network address is missing", nil)
	}
	var owner netip.Addr
	for _, candidate := range addresses {
		candidate = candidate.Unmap()
		if !candidate.Is4() {
			continue
		}
		if !candidate.IsValid() || candidate.Zone() != "" || !candidate.IsPrivate() || candidate.IsUnspecified() || candidate.IsLoopback() || candidate.IsMulticast() || candidate.IsLinkLocalUnicast() {
			return netip.Addr{}, model.NewError(model.CodeAmbiguous, "workspace inspected IPv4 address is not a private owner IP", nil)
		}
		if owner.IsValid() {
			return netip.Addr{}, model.NewError(model.CodeAmbiguous, "workspace has multiple inspected private IPv4 owner addresses", nil)
		}
		owner = candidate
	}
	if !owner.IsValid() {
		return netip.Addr{}, model.NewError(model.CodeAmbiguous, "workspace has no inspected private IPv4 owner address", nil)
	}
	return owner, nil
}

func normalizedHostBridgeEnvironmentBase(name string) (string, error) {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return "", fmt.Errorf("invalid host bridge name %q", name)
	}
	var normalized strings.Builder
	normalized.Grow(len(hostBridgeEnvironmentPrefix) + len(name))
	normalized.WriteString(hostBridgeEnvironmentPrefix)
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
			normalized.WriteByte(byte(character - 'a' + 'A'))
		case character >= '0' && character <= '9', character == '_':
			normalized.WriteRune(character)
		case character == '-':
			normalized.WriteByte('_')
		default:
			return "", fmt.Errorf("invalid host bridge name %q", name)
		}
	}
	return normalized.String(), nil
}

func validateHostBridgePlanEnvironmentSlots(execution plan.ExecutionPlan, bases map[string]string) error {
	reserved := make(map[string]struct{}, len(bases)*2)
	for base := range bases {
		reserved[base+"_HOST"] = struct{}{}
		reserved[base+"_PORT"] = struct{}{}
	}
	validate := func(command plan.ResolvedCommand) error {
		for _, grant := range command.Env {
			if _, collision := reserved[grant.Name]; collision {
				return model.NewError(model.CodeInvalidInput, fmt.Sprintf("approved command environment collides with reserved host bridge key %q", grant.Name), nil)
			}
		}
		return nil
	}
	for _, command := range execution.Setup {
		if err := validate(command); err != nil {
			return err
		}
	}
	for _, process := range execution.Processes {
		if err := validate(process.Command); err != nil {
			return err
		}
		if process.Health != nil && process.Health.Command != nil {
			if err := validate(*process.Health.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyHostBridgePlanEnvironment(execution plan.ExecutionPlan, environment map[string]string) (plan.ExecutionPlan, error) {
	if len(environment) == 0 {
		return execution, nil
	}
	keys := sortedHostBridgeEnvironmentKeys(environment)
	add := func(command plan.ResolvedCommand) (plan.ResolvedCommand, error) {
		copy := command
		copy.Env = append([]plan.EnvGrant(nil), command.Env...)
		for _, key := range keys {
			for _, existing := range command.Env {
				if existing.Name == key {
					return command, model.NewError(model.CodeInvalidInput, fmt.Sprintf("command environment collides with reserved host bridge key %q", key), nil)
				}
			}
			copy.Env = append(copy.Env, plan.EnvGrant{Name: key, Value: environment[key]})
		}
		return copy, nil
	}
	execution.Setup = append([]plan.ResolvedCommand(nil), execution.Setup...)
	for index := range execution.Setup {
		updated, err := add(execution.Setup[index])
		if err != nil {
			return execution, err
		}
		execution.Setup[index] = updated
	}
	execution.Processes = append([]plan.ResolvedProcess(nil), execution.Processes...)
	for index := range execution.Processes {
		updated, err := add(execution.Processes[index].Command)
		if err != nil {
			return execution, err
		}
		execution.Processes[index].Command = updated
		if execution.Processes[index].Health != nil {
			health := *execution.Processes[index].Health
			execution.Processes[index].Health = &health
			if health.Command != nil {
				command, commandErr := add(*health.Command)
				if commandErr != nil {
					return execution, commandErr
				}
				execution.Processes[index].Health.Command = &command
			}
		}
	}
	return execution, nil
}

func mergeHostBridgeEnvironment(environment, hostEnvironment map[string]string) (map[string]string, error) {
	merged := cloneEnvironment(environment)
	if len(hostEnvironment) == 0 {
		return merged, nil
	}
	if merged == nil {
		merged = make(map[string]string, len(hostEnvironment))
	}
	for key, value := range hostEnvironment {
		if _, collision := merged[key]; collision {
			return nil, model.NewError(model.CodeInvalidInput, fmt.Sprintf("environment collides with reserved host bridge key %q", key), nil)
		}
		merged[key] = value
	}
	return merged, nil
}

func validateReadyHostRelay(relay hostTCPRelay, listener netip.Addr, destination netip.AddrPort) error {
	if relay == nil {
		return model.NewError(model.CodeUnavailable, "host bridge relay returned no ready listener", nil)
	}
	address := relay.Addr()
	if !address.IsValid() || address.Addr().Unmap() != listener.Unmap() || address.Port() == 0 || relay.Destination() != destination {
		return model.NewError(model.CodeUnavailable, "host bridge relay readiness evidence differs from the approved grant", nil)
	}
	select {
	case <-relay.Done():
		return model.NewError(model.CodeUnavailable, "host bridge relay closed before readiness", relay.Err())
	default:
		return nil
	}
}

func (session *hostBridgeSession) Environment() map[string]string {
	if session == nil {
		return nil
	}
	return cloneEnvironment(session.environment)
}

func (session *hostBridgeSession) Close() error {
	if session == nil {
		return nil
	}
	var result error
	for index := len(session.relays) - 1; index >= 0; index-- {
		result = errors.Join(result, session.relays[index].Close())
	}
	session.relays = nil
	return result
}

func hasHostBridgeGrants(execution plan.ExecutionPlan) bool {
	for _, grant := range execution.Bridges {
		if grant.Kind == "host" {
			return true
		}
	}
	return false
}

func sortedHostBridgeEnvironmentKeys(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
