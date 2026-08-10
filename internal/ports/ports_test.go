package ports

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

var loopback = netip.MustParseAddr("127.0.0.1")

func TestPlanDefaultsToExactLoopbackAndReservesUntilRelease(t *testing.T) {
	publication, err := Plan([]plan.PortRequest{{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP}}, fallbackCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Abort() })

	requests, err := publication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].HostIP != loopback || requests[0].HostPort == nil || *requests[0].HostPort == 0 {
		t.Fatalf("requests = %#v, want one exact 127.0.0.1 fallback reservation", requests)
	}
	reservedPort := *requests[0].HostPort
	if listener, listenErr := listenOn(loopback, reservedPort); listenErr == nil {
		_ = listener.Close()
		t.Fatalf("reserved port %d was bindable before release", reservedPort)
	}

	requests[0].HostIP = netip.MustParseAddr("0.0.0.0")
	other := uint16(1)
	requests[0].HostPort = &other
	unchanged, err := publication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if unchanged[0].HostIP != loopback || *unchanged[0].HostPort != reservedPort {
		t.Fatalf("caller mutated planned request: %#v", unchanged[0])
	}

	forCreate, err := publication.ReleaseForCreate()
	if err != nil {
		t.Fatal(err)
	}
	if len(forCreate) != 0 {
		t.Fatalf("fallback returned native runtime ports: %#v", forCreate)
	}
	if listener, listenErr := listenOn(loopback, reservedPort); listenErr == nil {
		_ = listener.Close()
		t.Fatalf("reservation released before helper handoff")
	}
	relayBindings, err := publication.ReleaseForRelay()
	if err != nil {
		t.Fatal(err)
	}
	if len(relayBindings) != 1 || relayBindings[0].HostPort != reservedPort {
		t.Fatalf("relay bindings = %#v", relayBindings)
	}
	listener, err := listenOn(loopback, reservedPort)
	if err != nil {
		t.Fatalf("reservation remained held after helper handoff: %v", err)
	}
	_ = listener.Close()
	if _, err := publication.ReleaseForCreate(); !errors.Is(err, ErrReservationState) {
		t.Fatalf("second ReleaseForCreate error = %v", err)
	}
}

func TestPlanRequiresExplicitNonLoopbackGrant(t *testing.T) {
	wildcard := netip.MustParseAddr("0.0.0.0")
	request := plan.PortRequest{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP, HostIP: wildcard}
	if _, err := Plan([]plan.PortRequest{request}, nativeCapabilities()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ungranted wildcard error = %v", err)
	}

	request.ExplicitNonLoopbackGrant = true
	publication, err := Plan([]plan.PortRequest{request}, nativeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := publication.ReleaseForCreate()
	if err != nil {
		t.Fatal(err)
	}
	if requests[0].HostIP != wildcard || requests[0].HostPort == nil || *requests[0].HostPort != 0 {
		t.Fatalf("explicit non-loopback request = %#v", requests[0])
	}
}

func TestPlanRejectsUnsupportedProtocolDuplicateAndFixedConflicts(t *testing.T) {
	hostPort := freePort(t)
	tests := []struct {
		name     string
		requests []plan.PortRequest
		want     error
	}{
		{
			name:     "UDP",
			requests: []plan.PortRequest{{Name: "dns", GuestPort: 53, Protocol: "udp"}},
			want:     ErrInvalidRequest,
		},
		{
			name: "duplicate name",
			requests: []plan.PortRequest{
				{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP},
				{Name: "web", GuestPort: 3001, Protocol: ProtocolTCP},
			},
			want: ErrInvalidRequest,
		},
		{
			name: "duplicate fixed binding",
			requests: []plan.PortRequest{
				{Name: "api", GuestPort: 3000, Protocol: ProtocolTCP, HostIP: loopback, HostPort: &hostPort},
				{Name: "web", GuestPort: 3001, Protocol: ProtocolTCP, HostIP: loopback, HostPort: &hostPort},
			},
			want: ErrConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Plan(test.requests, fallbackCapabilities()); !errors.Is(err, test.want) {
				t.Fatalf("Plan error = %v, want %v", err, test.want)
			}
		})
	}

	occupied, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := uint16(occupied.Addr().(*net.TCPAddr).Port)
	_, err = Plan([]plan.PortRequest{{
		Name: "occupied", GuestPort: 3000, Protocol: ProtocolTCP, HostIP: loopback, HostPort: &occupiedPort,
	}}, fallbackCapabilities())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("occupied fixed port error = %v", err)
	}
}

func TestPlanSelectsNativeDynamicOnlyWhenCapabilityIsProven(t *testing.T) {
	request := []plan.PortRequest{{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP, HostIP: loopback}}
	native, err := Plan(request, nativeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	nativeRequests, err := native.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if nativeRequests[0].HostPort == nil || *nativeRequests[0].HostPort != 0 {
		t.Fatalf("native dynamic host port = %#v, want pointer to 0", nativeRequests[0].HostPort)
	}
	if _, err := native.ReleaseForCreate(); err != nil {
		t.Fatal(err)
	}

	fallback, err := Plan(request, fallbackCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fallback.Abort() })
	fallbackRequests, err := fallback.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if fallbackRequests[0].HostPort == nil || *fallbackRequests[0].HostPort == 0 {
		t.Fatalf("fallback host port = %#v, want reserved nonzero port", fallbackRequests[0].HostPort)
	}
}

func TestPlanUsesHelperFallbackWhenNativePublicationIsUnavailable(t *testing.T) {
	request := []plan.PortRequest{{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP}}
	publication, err := Plan(request, runtime.Capabilities{MachineReadableInspection: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Abort() })
	if runtimeRequests, err := publication.ReleaseForCreate(); err != nil || len(runtimeRequests) != 0 {
		t.Fatalf("fallback runtime requests = %#v, %v", runtimeRequests, err)
	}
	if _, err := publication.ReleaseForRelay(); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(request, runtime.Capabilities{FixedPublication: true}); !errors.Is(err, ErrPublicationUnsupported) {
		t.Fatalf("missing inspection error = %v", err)
	}
	if publication, err := Plan(nil, runtime.Capabilities{}); err != nil {
		t.Fatalf("empty publication set should not require capabilities: %v", err)
	} else if _, err := publication.ReleaseForCreate(); err != nil {
		t.Fatalf("empty publication release: %v", err)
	}
}

func TestFallbackAcceptsExplicitNonLoopbackBind(t *testing.T) {
	request := plan.PortRequest{
		Name: "public", GuestPort: 3000, Protocol: ProtocolTCP,
		HostIP: netip.MustParseAddr("0.0.0.0"), ExplicitNonLoopbackGrant: true,
	}
	publication, err := Plan([]plan.PortRequest{request}, runtime.Capabilities{MachineReadableInspection: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Abort() })
	requests, err := publication.RequestedBindings()
	if err != nil || len(requests) != 1 || requests[0].HostIP != request.HostIP {
		t.Fatalf("explicit fallback bind = %#v, %v", requests, err)
	}
}

func TestFallbackReservationsAreUniqueAndLifecycleIsOneShot(t *testing.T) {
	publication, err := Plan([]plan.PortRequest{
		{Name: "zeta", GuestPort: 3002, Protocol: ProtocolTCP},
		{Name: "alpha", GuestPort: 3000, Protocol: ProtocolTCP},
		{Name: "middle", GuestPort: 3001, Protocol: ProtocolTCP},
	}, fallbackCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Abort() })
	requests, err := publication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	if got := []uint16{requests[0].GuestPort, requests[1].GuestPort, requests[2].GuestPort}; !reflect.DeepEqual(got, []uint16{3000, 3001, 3002}) {
		t.Fatalf("deterministic request order = %v", got)
	}
	seen := make(map[uint16]struct{}, len(requests))
	for _, request := range requests {
		if request.HostIP != loopback || request.HostPort == nil || *request.HostPort == 0 {
			t.Fatalf("invalid fallback request: %#v", request)
		}
		if _, exists := seen[*request.HostPort]; exists {
			t.Fatalf("duplicate fallback port %d", *request.HostPort)
		}
		seen[*request.HostPort] = struct{}{}
	}
	if _, err := publication.Reconcile(nil); !errors.Is(err, ErrReservationState) {
		t.Fatalf("reconciliation before release error = %v", err)
	}
	if _, err := publication.ReleaseForCreate(); err != nil {
		t.Fatal(err)
	}
	if _, err := publication.ReleaseForRelay(); err != nil {
		t.Fatal(err)
	}
	if err := publication.Abort(); !errors.Is(err, ErrReservationState) {
		t.Fatalf("abort after release error = %v", err)
	}
}

func TestAbortReleasesReservationAndRejectsReuse(t *testing.T) {
	publication, err := Plan([]plan.PortRequest{{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP}}, fallbackCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := publication.RequestedBindings()
	if err != nil {
		t.Fatal(err)
	}
	reservedPort := *requests[0].HostPort
	if err := publication.Abort(); err != nil {
		t.Fatal(err)
	}
	listener, err := listenOn(loopback, reservedPort)
	if err != nil {
		t.Fatalf("aborted reservation remained held: %v", err)
	}
	_ = listener.Close()
	if err := publication.Abort(); !errors.Is(err, ErrReservationState) {
		t.Fatalf("second abort error = %v", err)
	}
	if _, err := publication.ReleaseForCreate(); !errors.Is(err, ErrReservationState) {
		t.Fatalf("release after abort error = %v", err)
	}
}

func TestReconcileRequiresExactInspectedBindings(t *testing.T) {
	hostPort := freePort(t)
	baseRequest := plan.PortRequest{
		Name: "web", GuestPort: 3000, Protocol: ProtocolTCP, HostIP: loopback, HostPort: &hostPort,
	}
	baseObserved := runtime.PortBinding{HostIP: loopback, HostPort: hostPort, GuestPort: 3000, Protocol: ProtocolTCP}
	tests := []struct {
		name   string
		mutate func(*runtime.PortBinding)
	}{
		{name: "host", mutate: func(binding *runtime.PortBinding) { binding.HostIP = netip.MustParseAddr("127.0.0.2") }},
		{name: "host port", mutate: func(binding *runtime.PortBinding) { binding.HostPort++ }},
		{name: "guest port", mutate: func(binding *runtime.PortBinding) { binding.GuestPort++ }},
		{name: "protocol", mutate: func(binding *runtime.PortBinding) { binding.Protocol = "udp" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publication, err := Plan([]plan.PortRequest{baseRequest}, fallbackCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := publication.ReleaseForCreate(); err != nil {
				t.Fatal(err)
			}
			observed := baseObserved
			test.mutate(&observed)
			if _, err := publication.Reconcile([]runtime.PortBinding{observed}); !errors.Is(err, ErrBindingMismatch) {
				t.Fatalf("Reconcile error = %v", err)
			}
		})
	}

	publication, err := Plan([]plan.PortRequest{baseRequest}, fallbackCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publication.ReleaseForCreate(); err != nil {
		t.Fatal(err)
	}
	bindings, err := publication.Reconcile([]runtime.PortBinding{baseObserved})
	if err != nil {
		t.Fatal(err)
	}
	want := []PublishedBinding{{Name: "web", HostIP: loopback, HostPort: hostPort, GuestPort: 3000, Protocol: ProtocolTCP}}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("bindings = %#v, want %#v", bindings, want)
	}
	if _, err := publication.Reconcile([]runtime.PortBinding{baseObserved}); !errors.Is(err, ErrReservationState) {
		t.Fatalf("second reconciliation error = %v", err)
	}
}

func TestReconcileAcceptsOnlyNonzeroNativeDynamicAllocation(t *testing.T) {
	publication, err := Plan([]plan.PortRequest{{Name: "web", GuestPort: 3000, Protocol: ProtocolTCP}}, nativeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publication.ReleaseForCreate(); err != nil {
		t.Fatal(err)
	}
	zero := runtime.PortBinding{HostIP: loopback, HostPort: 0, GuestPort: 3000, Protocol: ProtocolTCP}
	if _, err := publication.Reconcile([]runtime.PortBinding{zero}); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("zero native allocation error = %v", err)
	}
	zero.HostPort = 49152
	bindings, err := publication.Reconcile([]runtime.PortBinding{zero})
	if err != nil {
		t.Fatal(err)
	}
	if bindings[0].HostPort != 49152 {
		t.Fatalf("observed host port = %d", bindings[0].HostPort)
	}
}

func TestReconcileExistingFixedAndDynamicBindings(t *testing.T) {
	occupied, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	fixedPort := uint16(occupied.Addr().(*net.TCPAddr).Port)
	requests := []plan.PortRequest{
		{Name: "zeta", HostIP: loopback, HostPort: &fixedPort, GuestPort: 3001, Protocol: ProtocolTCP},
		{Name: "alpha", GuestPort: 3000, Protocol: ProtocolTCP},
	}
	inspected := []runtime.PortBinding{
		{HostIP: loopback, HostPort: fixedPort, GuestPort: 3001, Protocol: ProtocolTCP},
		{HostIP: loopback, HostPort: 49152, GuestPort: 3000, Protocol: ProtocolTCP},
	}
	slices.Reverse(inspected)

	want := []PublishedBinding{
		{Name: "alpha", HostIP: loopback, HostPort: 49152, GuestPort: 3000, Protocol: ProtocolTCP},
		{Name: "zeta", HostIP: loopback, HostPort: fixedPort, GuestPort: 3001, Protocol: ProtocolTCP},
	}
	bindings, err := ReconcileExisting(requests, want, inspected, nativeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("bindings = %#v, want %#v", bindings, want)
	}
}

func TestReconcileExistingRejectsInspectedMismatch(t *testing.T) {
	fixedPort := uint16(8080)
	requests := []plan.PortRequest{
		{Name: "fixed", HostIP: loopback, HostPort: &fixedPort, GuestPort: 3000, Protocol: ProtocolTCP},
		{Name: "dynamic", HostIP: loopback, GuestPort: 3001, Protocol: ProtocolTCP},
	}
	base := []runtime.PortBinding{
		{HostIP: loopback, HostPort: 8080, GuestPort: 3000, Protocol: ProtocolTCP},
		{HostIP: loopback, HostPort: 49152, GuestPort: 3001, Protocol: ProtocolTCP},
	}
	tests := []struct {
		name   string
		mutate func([]runtime.PortBinding) []runtime.PortBinding
	}{
		{name: "count", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding { return bindings[:1] }},
		{name: "IP", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding {
			bindings[0].HostIP = netip.MustParseAddr("127.0.0.2")
			return bindings
		}},
		{name: "guest", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding {
			bindings[0].GuestPort++
			return bindings
		}},
		{name: "protocol", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding {
			bindings[0].Protocol = "udp"
			return bindings
		}},
		{name: "fixed host port", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding {
			bindings[0].HostPort++
			return bindings
		}},
		{name: "dynamic zero", mutate: func(bindings []runtime.PortBinding) []runtime.PortBinding {
			bindings[1].HostPort = 0
			return bindings
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := append([]runtime.PortBinding(nil), base...)
			recorded := []PublishedBinding{
				{Name: "dynamic", HostIP: loopback, HostPort: 49152, GuestPort: 3001, Protocol: ProtocolTCP},
				{Name: "fixed", HostIP: loopback, HostPort: 8080, GuestPort: 3000, Protocol: ProtocolTCP},
			}
			if _, err := ReconcileExisting(requests, recorded, test.mutate(observed), nativeCapabilities()); !errors.Is(err, ErrBindingMismatch) {
				t.Fatalf("ReconcileExisting error = %v", err)
			}
		})
	}
}

func TestRenderURLsIsDeterministicAndIPv6Safe(t *testing.T) {
	bindings := []PublishedBinding{
		{Name: "zeta", HostIP: loopback, HostPort: 8080, GuestPort: 3000, Protocol: ProtocolTCP},
		{Name: "alpha", HostIP: netip.MustParseAddr("2001:db8::1"), HostPort: 8443, GuestPort: 443, Protocol: ProtocolTCP},
	}
	want := []string{"http://[2001:db8::1]:8443", "http://127.0.0.1:8080"}
	for permutation := range permutations(bindings) {
		got, err := RenderURLs(permutation)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("RenderURLs(%#v) = %#v, want %#v", permutation, got, want)
		}
	}
}

func TestExplicitIPv6GrantReconcilesAndRenders(t *testing.T) {
	address := netip.MustParseAddr("2001:db8::1")
	publication, err := Plan([]plan.PortRequest{{
		Name: "web", GuestPort: 3000, Protocol: ProtocolTCP,
		HostIP: address, ExplicitNonLoopbackGrant: true,
	}}, nativeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publication.ReleaseForCreate(); err != nil {
		t.Fatal(err)
	}
	bindings, err := publication.Reconcile([]runtime.PortBinding{{
		HostIP: address, HostPort: 49152, GuestPort: 3000, Protocol: ProtocolTCP,
	}})
	if err != nil {
		t.Fatal(err)
	}
	urls, err := RenderURLs(bindings)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http://[2001:db8::1]:49152"}; !reflect.DeepEqual(urls, want) {
		t.Fatalf("URLs = %#v, want %#v", urls, want)
	}
}

func TestConcurrentFallbackReservationsAreUnique(t *testing.T) {
	const count = 32
	plans := make([]*PublicationPlan, count)
	ports := make([]uint16, count)
	errorsByIndex := make([]error, count)
	var wait sync.WaitGroup
	wait.Add(count)
	for index := 0; index < count; index++ {
		go func(index int) {
			defer wait.Done()
			publication, err := Plan([]plan.PortRequest{{Name: "web", GuestPort: uint16(3000 + index), Protocol: ProtocolTCP}}, fallbackCapabilities())
			if err != nil {
				errorsByIndex[index] = err
				return
			}
			plans[index] = publication
			requests, err := publication.RequestedBindings()
			if err != nil {
				errorsByIndex[index] = err
				return
			}
			ports[index] = *requests[0].HostPort
		}(index)
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("reservation %d: %v", index, err)
		}
	}
	ordered := append([]uint16(nil), ports...)
	slices.Sort(ordered)
	for index := 1; index < len(ordered); index++ {
		if ordered[index] == ordered[index-1] {
			t.Fatalf("concurrent reservations reused port %d", ordered[index])
		}
	}

	wait.Add(count)
	for index := range plans {
		go func(publication *PublicationPlan) {
			defer wait.Done()
			_, _ = publication.ReleaseForCreate()
			_, _ = publication.ReleaseForRelay()
		}(plans[index])
	}
	wait.Wait()
}

func fallbackCapabilities() runtime.Capabilities {
	return runtime.Capabilities{FixedPublication: true, MachineReadableInspection: true}
}

func nativeCapabilities() runtime.Capabilities {
	return runtime.Capabilities{FixedPublication: true, DynamicPublication: true, MachineReadableInspection: true}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func listenOn(address netip.Addr, port uint16) (*net.TCPListener, error) {
	return net.ListenTCP(tcpNetwork(address), &net.TCPAddr{IP: net.IP(address.AsSlice()), Port: int(port)})
}

func permutations(bindings []PublishedBinding) func(func([]PublishedBinding) bool) {
	return func(yield func([]PublishedBinding) bool) {
		if !yield(append([]PublishedBinding(nil), bindings...)) {
			return
		}
		reversed := append([]PublishedBinding(nil), bindings...)
		slices.Reverse(reversed)
		yield(reversed)
	}
}
