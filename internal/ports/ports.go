package ports

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"

	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

const (
	ProtocolTCP         = "tcp"
	MaxPublications     = 128
	maxReservationTries = 128
)

var (
	ErrPublicationUnsupported = errors.New("port publication unsupported by the proven runtime capabilities")
	ErrInvalidRequest         = errors.New("invalid port publication request")
	ErrConflict               = errors.New("host port conflict")
	ErrReservationState       = errors.New("invalid port reservation state")
	ErrBindingMismatch        = errors.New("inspected port bindings do not match the publication plan")
)

var defaultHostIP = netip.MustParseAddr("127.0.0.1")

type publicationState uint8

const (
	stateReserved publicationState = iota
	stateReleased
	stateReconciled
	stateAborted
)

type plannedPort struct {
	name           string
	request        runtime.PortRequest
	runtimeDynamic bool
}

// PublicationPlan holds exact fallback listeners through workspace creation
// and inspection. Native mode releases directly to the runtime; fallback mode
// releases only immediately before the persistent helper binds.
type PublicationPlan struct {
	mu             sync.Mutex
	ports          []plannedPort
	listeners      []net.Listener
	fallback       bool
	createReleased bool
	state          publicationState
}

// PublishedBinding associates runtime-inspected truth with the resolved port
// name. Values in this type are never inferred from preflight checks.
type PublishedBinding struct {
	Name      string
	HostIP    netip.Addr
	HostPort  uint16
	GuestPort uint16
	Protocol  string
}

// Plan validates every request before runtime mutation. Native publication is
// used only when all requested fixed/dynamic operations are proven; otherwise
// DSX reserves exact approved host listeners for its helper fallback.
func Plan(requests []plan.PortRequest, capabilities runtime.Capabilities) (*PublicationPlan, error) {
	result := &PublicationPlan{state: stateReserved}
	if len(requests) == 0 {
		return result, nil
	}
	if len(requests) > MaxPublications {
		return nil, fmt.Errorf("%w: %d publications exceeds limit %d", ErrInvalidRequest, len(requests), MaxPublications)
	}
	if !capabilities.MachineReadableInspection {
		return nil, fmt.Errorf("%w: machine-readable inspection is not proven", ErrPublicationUnsupported)
	}
	validated, err := validate(requests)
	if err != nil {
		return nil, err
	}
	native := capabilities.FixedPublication
	for _, port := range validated {
		if port.request.HostPort == nil && !capabilities.DynamicPublication {
			native = false
		}
	}
	result.fallback = !native
	result.ports = make([]plannedPort, 0, len(validated))
	if native {
		if err := preflightFixed(validated); err != nil {
			return nil, err
		}
		for _, port := range validated {
			if port.request.HostPort == nil {
				zero := uint16(0)
				port.request.HostPort = &zero
				port.runtimeDynamic = true
			}
			result.ports = append(result.ports, port)
		}
		return result, nil
	}
	for _, port := range validated {
		listener, hostPort, reserveErr := reserveExact(port.request.HostIP, port.request.HostPort)
		if reserveErr != nil {
			_ = closeListeners(result.listeners)
			return nil, fmt.Errorf("%w: publication %q: %v", ErrConflict, port.name, reserveErr)
		}
		port.request.HostPort = &hostPort
		result.listeners = append(result.listeners, listener)
		result.ports = append(result.ports, port)
	}
	return result, nil
}

func validate(requests []plan.PortRequest) ([]plannedPort, error) {
	validated := make([]plannedPort, 0, len(requests))
	names := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if !validPublicationName(request.Name) {
			return nil, fmt.Errorf("%w: publication %d has invalid name %q", ErrInvalidRequest, index, request.Name)
		}
		if _, exists := names[request.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate publication name %q", ErrInvalidRequest, request.Name)
		}
		names[request.Name] = struct{}{}
		if request.GuestPort == 0 {
			return nil, fmt.Errorf("%w: publication %q has guest port 0", ErrInvalidRequest, request.Name)
		}
		if request.Protocol != ProtocolTCP {
			return nil, fmt.Errorf("%w: publication %q uses unsupported protocol %q", ErrInvalidRequest, request.Name, request.Protocol)
		}
		hostIP := request.HostIP
		if !hostIP.IsValid() {
			hostIP = defaultHostIP
		}
		if hostIP.Zone() != "" || hostIP.Is4In6() || hostIP.IsMulticast() || hostIP.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("%w: publication %q has unsupported bind address %q", ErrInvalidRequest, request.Name, hostIP)
		}
		if !hostIP.IsLoopback() && !request.ExplicitNonLoopbackGrant {
			return nil, fmt.Errorf("%w: publication %q requires an explicit non-loopback grant for %s", ErrInvalidRequest, request.Name, hostIP)
		}
		if request.HostPort != nil && *request.HostPort == 0 {
			return nil, fmt.Errorf("%w: publication %q must express a dynamic host port as nil", ErrInvalidRequest, request.Name)
		}
		var hostPort *uint16
		if request.HostPort != nil {
			value := *request.HostPort
			hostPort = &value
		}
		validated = append(validated, plannedPort{
			name: request.Name,
			request: runtime.PortRequest{
				HostIP: hostIP, HostPort: hostPort, GuestPort: request.GuestPort, Protocol: request.Protocol,
			},
		})
	}
	slices.SortFunc(validated, func(left, right plannedPort) int {
		return comparePlanned(left, right)
	})
	for left := 0; left < len(validated); left++ {
		leftPort := validated[left].request.HostPort
		if leftPort == nil {
			continue
		}
		for right := left + 1; right < len(validated); right++ {
			rightPort := validated[right].request.HostPort
			if rightPort != nil && *leftPort == *rightPort && addressesOverlap(validated[left].request.HostIP, validated[right].request.HostIP) {
				return nil, fmt.Errorf("%w: publications %q and %q both request TCP port %d on overlapping bind addresses", ErrConflict, validated[left].name, validated[right].name, *leftPort)
			}
		}
	}
	return validated, nil
}

func validPublicationName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func preflightFixed(ports []plannedPort) error {
	for _, port := range ports {
		if port.request.HostPort == nil {
			continue
		}
		listener, _, err := reserveExact(port.request.HostIP, port.request.HostPort)
		if err != nil {
			return fmt.Errorf("%w: publication %q cannot bind %s: %v", ErrConflict, port.name, net.JoinHostPort(port.request.HostIP.String(), strconv.Itoa(int(*port.request.HostPort))), err)
		}
		if err := listener.Close(); err != nil {
			return fmt.Errorf("%w: close fixed-port preflight for publication %q: %w", ErrConflict, port.name, err)
		}
	}
	return nil
}

func reserveExact(address netip.Addr, port *uint16) (net.Listener, uint16, error) {
	requested := uint16(0)
	if port != nil {
		requested = *port
	}
	listener, err := net.ListenTCP(tcpNetwork(address), &net.TCPAddr{IP: net.IP(address.AsSlice()), Port: int(requested)})
	if err != nil {
		return nil, 0, err
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || bound.Port <= 0 || bound.Port > 65535 {
		_ = listener.Close()
		return nil, 0, errors.New("reservation returned an invalid TCP address")
	}
	resolved := uint16(bound.Port)
	if requested != 0 && resolved != requested {
		_ = listener.Close()
		return nil, 0, errors.New("reservation returned a different fixed port")
	}
	return listener, resolved, nil
}

// RequestedBindings returns a defensive copy while reservations are held. It
// is for displaying the pending create request, not for authorizing creation.
func (publication *PublicationPlan) RequestedBindings() ([]runtime.PortRequest, error) {
	if publication == nil {
		return nil, fmt.Errorf("%w: nil publication plan", ErrReservationState)
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if publication.state != stateReserved || publication.createReleased {
		return nil, fmt.Errorf("%w: requested bindings are unavailable after release", ErrReservationState)
	}
	return cloneRequests(publication.ports), nil
}

// ReleaseForCreate returns native runtime requests. In fallback mode it returns
// zero requests and deliberately keeps every exact reservation held.
func (publication *PublicationPlan) ReleaseForCreate() ([]runtime.PortRequest, error) {
	if publication == nil {
		return nil, fmt.Errorf("%w: nil publication plan", ErrReservationState)
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if publication.state != stateReserved || publication.createReleased {
		return nil, fmt.Errorf("%w: publication plan was already released or aborted", ErrReservationState)
	}
	publication.createReleased = true
	if publication.fallback {
		return nil, nil
	}
	publication.state = stateReleased
	return cloneRequests(publication.ports), nil
}

// ReleaseForRelay closes fallback reservations immediately before helper
// Ensure. It returns the exact resolved named listeners the helper must bind.
func (publication *PublicationPlan) ReleaseForRelay() ([]PublishedBinding, error) {
	if publication == nil {
		return nil, fmt.Errorf("%w: nil publication plan", ErrReservationState)
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if !publication.fallback {
		if publication.state != stateReleased {
			return nil, fmt.Errorf("%w: native publication has not been released for create", ErrReservationState)
		}
		return nil, nil
	}
	if publication.state != stateReserved || !publication.createReleased {
		return nil, fmt.Errorf("%w: fallback reservations were already released or aborted", ErrReservationState)
	}
	if err := closeListeners(publication.listeners); err != nil {
		publication.state = stateAborted
		publication.listeners = nil
		return nil, fmt.Errorf("%w: release exact helper reservations: %w", ErrReservationState, err)
	}
	publication.listeners = nil
	publication.state = stateReleased
	return plannedBindings(publication.ports), nil
}

// Abort closes held reservations without authorizing a runtime create.
func (publication *PublicationPlan) Abort() error {
	if publication == nil {
		return fmt.Errorf("%w: nil publication plan", ErrReservationState)
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if publication.state != stateReserved {
		return fmt.Errorf("%w: publication plan was already released or aborted", ErrReservationState)
	}
	publication.state = stateAborted
	err := closeListeners(publication.listeners)
	publication.listeners = nil
	if err != nil {
		return fmt.Errorf("%w: abort loopback reservations: %w", ErrReservationState, err)
	}
	return nil
}

func UsesFallback(requests []plan.PortRequest, capabilities runtime.Capabilities) bool {
	if len(requests) == 0 || !capabilities.FixedPublication {
		return len(requests) != 0
	}
	for _, request := range requests {
		if request.HostPort == nil && !capabilities.DynamicPublication {
			return true
		}
	}
	return false
}

// ReconcileExisting binds a requested plan to durable exact named listener
// evidence and to native runtime inspection when native publication is proven.
func ReconcileExisting(requests []plan.PortRequest, recorded []PublishedBinding, inspected []runtime.PortBinding, capabilities runtime.Capabilities) ([]PublishedBinding, error) {
	validated, err := validate(requests)
	if err != nil {
		return nil, err
	}
	native := capabilities.FixedPublication
	for _, port := range validated {
		if port.request.HostPort == nil && !capabilities.DynamicPublication {
			native = false
		}
	}
	exact, err := reconcileRecorded(validated, recorded)
	if err != nil {
		return nil, err
	}
	if !native {
		if len(inspected) != 0 {
			return nil, fmt.Errorf("%w: fallback workspace unexpectedly has native published ports", ErrBindingMismatch)
		}
		return exact, nil
	}
	for index := range validated {
		validated[index].runtimeDynamic = validated[index].request.HostPort == nil
	}
	observed, err := reconcilePorts(validated, inspected)
	if err != nil {
		return nil, err
	}
	if !equalBindings(exact, observed) {
		return nil, fmt.Errorf("%w: native inspection differs from durable listener evidence", ErrBindingMismatch)
	}
	return exact, nil
}

// Reconcile accepts only helper-ready fallback truth or complete native
// runtime-inspected truth.
func (publication *PublicationPlan) Reconcile(inspected []runtime.PortBinding) ([]PublishedBinding, error) {
	if publication == nil {
		return nil, fmt.Errorf("%w: nil publication plan", ErrReservationState)
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if publication.state != stateReleased {
		return nil, fmt.Errorf("%w: reconciliation requires released listeners", ErrReservationState)
	}
	var bindings []PublishedBinding
	var err error
	if publication.fallback {
		if len(inspected) != 0 {
			return nil, fmt.Errorf("%w: fallback workspace unexpectedly has native published ports", ErrBindingMismatch)
		}
		bindings = plannedBindings(publication.ports)
	} else {
		bindings, err = reconcilePorts(publication.ports, inspected)
	}
	if err != nil {
		return nil, err
	}
	publication.state = stateReconciled
	return bindings, nil
}
func reconcilePorts(expectedPorts []plannedPort, inspected []runtime.PortBinding) ([]PublishedBinding, error) {
	if len(inspected) != len(expectedPorts) {
		return nil, fmt.Errorf("%w: inspected %d bindings, want %d", ErrBindingMismatch, len(inspected), len(expectedPorts))
	}
	observed := append([]runtime.PortBinding(nil), inspected...)
	slices.SortFunc(observed, compareRuntimeBindings)
	used := make([]bool, len(observed))
	bindings := make([]PublishedBinding, 0, len(expectedPorts))
	for _, expected := range expectedPorts {
		match := -1
		for index, candidate := range observed {
			if used[index] || !bindingMatches(expected, candidate) {
				continue
			}
			match = index
			break
		}
		if match < 0 {
			return nil, fmt.Errorf("%w: no exact inspected binding for publication %q", ErrBindingMismatch, expected.name)
		}
		used[match] = true
		candidate := observed[match]
		bindings = append(bindings, PublishedBinding{
			Name: expected.name, HostIP: candidate.HostIP, HostPort: candidate.HostPort,
			GuestPort: candidate.GuestPort, Protocol: candidate.Protocol,
		})
	}
	return bindings, nil
}

func reconcileRecorded(expected []plannedPort, recorded []PublishedBinding) ([]PublishedBinding, error) {
	if len(recorded) != len(expected) {
		return nil, fmt.Errorf("%w: recorded %d bindings, want %d", ErrBindingMismatch, len(recorded), len(expected))
	}
	ordered := append([]PublishedBinding(nil), recorded...)
	slices.SortFunc(ordered, comparePublishedBindings)
	for index, port := range expected {
		binding := ordered[index]
		if binding.Name != port.name || binding.HostIP != port.request.HostIP || binding.HostPort == 0 || binding.GuestPort != port.request.GuestPort || binding.Protocol != port.request.Protocol {
			return nil, fmt.Errorf("%w: durable binding differs for publication %q", ErrBindingMismatch, port.name)
		}
		if port.request.HostPort != nil && binding.HostPort != *port.request.HostPort {
			return nil, fmt.Errorf("%w: durable fixed binding differs for publication %q", ErrBindingMismatch, port.name)
		}
	}
	return ordered, nil
}

func plannedBindings(ports []plannedPort) []PublishedBinding {
	result := make([]PublishedBinding, len(ports))
	for index, port := range ports {
		result[index] = PublishedBinding{
			Name: port.name, HostIP: port.request.HostIP, HostPort: pointerValue(port.request.HostPort),
			GuestPort: port.request.GuestPort, Protocol: port.request.Protocol,
		}
	}
	return result
}

func equalBindings(first, second []PublishedBinding) bool {
	if len(first) != len(second) {
		return false
	}
	left := append([]PublishedBinding(nil), first...)
	right := append([]PublishedBinding(nil), second...)
	slices.SortFunc(left, comparePublishedBindings)
	slices.SortFunc(right, comparePublishedBindings)
	return slices.Equal(left, right)
}

// RenderURLs renders inspected, reconciled bindings in a stable order. The
// HTTP scheme is the current user-facing URL contract; net.JoinHostPort keeps
// explicitly granted IPv6 binds valid.
func RenderURLs(bindings []PublishedBinding) ([]string, error) {
	ordered := append([]PublishedBinding(nil), bindings...)
	for _, binding := range ordered {
		if binding.Name == "" || !binding.HostIP.IsValid() || binding.HostIP.Zone() != "" || binding.HostPort == 0 || binding.GuestPort == 0 || binding.Protocol != ProtocolTCP {
			return nil, fmt.Errorf("%w: cannot render invalid inspected binding for %q", ErrBindingMismatch, binding.Name)
		}
	}
	slices.SortFunc(ordered, comparePublishedBindings)
	result := make([]string, len(ordered))
	for index, binding := range ordered {
		result[index] = (&url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(binding.HostIP.String(), strconv.Itoa(int(binding.HostPort))),
		}).String()
	}
	return result, nil
}

func bindingMatches(expected plannedPort, observed runtime.PortBinding) bool {
	if !observed.HostIP.IsValid() || observed.HostIP != expected.request.HostIP || observed.HostPort == 0 || observed.GuestPort != expected.request.GuestPort || observed.Protocol != expected.request.Protocol {
		return false
	}
	return expected.runtimeDynamic || (expected.request.HostPort != nil && observed.HostPort == *expected.request.HostPort)
}

func cloneRequests(ports []plannedPort) []runtime.PortRequest {
	result := make([]runtime.PortRequest, len(ports))
	for index, port := range ports {
		result[index] = port.request
		if port.request.HostPort != nil {
			value := *port.request.HostPort
			result[index].HostPort = &value
		}
	}
	return result
}

func closeListeners(listeners []net.Listener) error {
	var result error
	for _, listener := range listeners {
		result = errors.Join(result, listener.Close())
	}
	return result
}

func addressesOverlap(left, right netip.Addr) bool {
	if left == right {
		return true
	}
	if left.IsUnspecified() || right.IsUnspecified() {
		return true
	}
	return false
}

func tcpNetwork(address netip.Addr) string {
	if address.Is4() {
		return "tcp4"
	}
	return "tcp6"
}

func comparePlanned(left, right plannedPort) int {
	if result := compareString(left.name, right.name); result != 0 {
		return result
	}
	return compareRuntimeRequests(left.request, right.request)
}

func compareRuntimeRequests(left, right runtime.PortRequest) int {
	if result := left.HostIP.Compare(right.HostIP); result != 0 {
		return result
	}
	if result := compareUint16(pointerValue(left.HostPort), pointerValue(right.HostPort)); result != 0 {
		return result
	}
	if result := compareUint16(left.GuestPort, right.GuestPort); result != 0 {
		return result
	}
	return compareString(left.Protocol, right.Protocol)
}

func compareRuntimeBindings(left, right runtime.PortBinding) int {
	if result := left.HostIP.Compare(right.HostIP); result != 0 {
		return result
	}
	if result := compareUint16(left.HostPort, right.HostPort); result != 0 {
		return result
	}
	if result := compareUint16(left.GuestPort, right.GuestPort); result != 0 {
		return result
	}
	return compareString(left.Protocol, right.Protocol)
}

func comparePublishedBindings(left, right PublishedBinding) int {
	if result := compareString(left.Name, right.Name); result != 0 {
		return result
	}
	return compareRuntimeBindings(
		runtime.PortBinding{HostIP: left.HostIP, HostPort: left.HostPort, GuestPort: left.GuestPort, Protocol: left.Protocol},
		runtime.PortBinding{HostIP: right.HostIP, HostPort: right.HostPort, GuestPort: right.GuestPort, Protocol: right.Protocol},
	)
}

func compareString(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareUint16(left, right uint16) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func pointerValue(value *uint16) uint16 {
	if value == nil {
		return 0
	}
	return *value
}
