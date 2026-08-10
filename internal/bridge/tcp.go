package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxTCPLease = 24 * time.Hour

var (
	ErrInvalidTCPGrant       = errors.New("invalid TCP grant")
	ErrTCPDestinationChanged = errors.New("TCP destination changed")
)

var sharedIPv4 = netip.MustParsePrefix("100.64.0.0/10")

// TCPResolver is the DNS operation used to pin a configured destination.
type TCPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// TCPGrant authorizes one temporary listener, one workspace source, and one
// already-resolved destination. DestinationLiteral records whether the
// configured destination was an IP literal; it does not relax destination
// address policy.
type TCPGrant struct {
	ListenerIP         netip.Addr
	ListenerPort       uint16
	OwnerIP            netip.Addr
	Destination        netip.AddrPort
	DestinationLiteral bool
	AllowRemotePeers   bool
	Lease              time.Duration
}

// TCPCounters contains connection lifecycle counts only. It intentionally has
// no byte, payload, host-name, or address fields.
type TCPCounters struct {
	Accepted     uint64
	Denied       uint64
	DialFailures uint64
	Active       uint64
	Completed    uint64
}

// TCPRelay is an in-process, destination-specific TCP relay.
type TCPRelay struct {
	listener    net.Listener
	address     netip.AddrPort
	destination netip.AddrPort
	cancel      context.CancelFunc
	dial        func(context.Context, string, string) (net.Conn, error)

	accepted     atomic.Uint64
	denied       atomic.Uint64
	dialFailures atomic.Uint64
	active       atomic.Uint64
	completed    atomic.Uint64

	mu       sync.Mutex
	stopping bool
	conns    map[net.Conn]struct{}
	err      error

	acceptDone chan struct{}
	done       chan struct{}
	handlers   sync.WaitGroup
	finishOnce sync.Once
}

type tcpDependencies struct {
	allowLoopback bool
	publication   bool
	routeSource   func(context.Context, netip.Addr) (netip.Addr, error)
	listen        func(context.Context, string, string) (net.Listener, error)
	dial          func(context.Context, string, string) (net.Conn, error)
}

// ResolveTCPDestination resolves a name exactly once and returns one pinned
// address. Numeric literals bypass DNS. DNS names which produce zero or more
// than one answer are rejected rather than selected by ordering.
func ResolveTCPDestination(ctx context.Context, resolver TCPResolver, host string, port int) (netip.AddrPort, error) {
	if ctx == nil {
		return netip.AddrPort{}, fmt.Errorf("%w: nil context", ErrInvalidTCPGrant)
	}
	if host == "" || strings.TrimSpace(host) != host {
		return netip.AddrPort{}, fmt.Errorf("%w: destination host is empty or has surrounding whitespace", ErrInvalidTCPGrant)
	}
	if port < 1 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("%w: destination port must be between 1 and 65535", ErrInvalidTCPGrant)
	}

	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if err := validateDestination(literal, false); err != nil {
			return netip.AddrPort{}, err
		}
		return netip.AddrPortFrom(literal, uint16(port)), nil
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}
	answers, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve TCP destination %q: %w", host, err)
	}
	if len(answers) != 1 {
		return netip.AddrPort{}, fmt.Errorf("%w: destination %q returned %d answers; exactly one is required", ErrInvalidTCPGrant, host, len(answers))
	}
	address := answers[0].Unmap()
	if err := validateDestination(address, false); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(address, uint16(port)), nil
}

// RevalidateTCPDestination fails if a fresh exact resolution no longer equals
// an approved pinned destination. It never substitutes the new answer.
func RevalidateTCPDestination(ctx context.Context, resolver TCPResolver, host string, expected netip.AddrPort) error {
	if !expected.IsValid() || expected.Port() == 0 {
		return fmt.Errorf("%w: expected destination is invalid", ErrInvalidTCPGrant)
	}
	resolved, err := ResolveTCPDestination(ctx, resolver, host, int(expected.Port()))
	if err != nil {
		return err
	}
	if resolved != netip.AddrPortFrom(expected.Addr().Unmap(), expected.Port()) {
		return fmt.Errorf("%w: approved %s no longer matches the configured name", ErrTCPDestinationChanged, expected)
	}
	return nil
}

// RouteSourceAddr asks the kernel which local address routes to the owner
// workspace IP. A connected UDP socket performs route selection without
// sending payload or invoking an external command.
func RouteSourceAddr(ctx context.Context, ownerIP netip.Addr) (netip.Addr, error) {
	return routeSourceAddr(ctx, ownerIP, kernelRouteProbe)
}

type routeProbe func(context.Context, string, string) (net.Addr, error)

func kernelRouteProbe(ctx context.Context, network, address string) (net.Addr, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	local := conn.LocalAddr()
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return local, nil
}

func routeSourceAddr(ctx context.Context, ownerIP netip.Addr, probe routeProbe) (netip.Addr, error) {
	if ctx == nil {
		return netip.Addr{}, fmt.Errorf("%w: nil context", ErrInvalidTCPGrant)
	}
	ownerIP = ownerIP.Unmap()
	if !ownerIP.IsValid() || ownerIP.Zone() != "" || ownerIP.IsUnspecified() || ownerIP.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("%w: owner workspace IP is unsupported", ErrInvalidTCPGrant)
	}
	if probe == nil {
		return netip.Addr{}, fmt.Errorf("%w: route probe is unavailable", ErrInvalidTCPGrant)
	}
	network := "udp6"
	if ownerIP.Is4() {
		network = "udp4"
	}
	local, err := probe(ctx, network, netip.AddrPortFrom(ownerIP, 9).String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("derive route source for owner %s: %w", ownerIP, err)
	}
	udp, ok := local.(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("%w: route probe returned %T, not UDP", ErrInvalidTCPGrant, local)
	}
	address, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("%w: route probe returned an invalid local IP", ErrInvalidTCPGrant)
	}
	address = address.Unmap()
	if address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() || address.Is4() != ownerIP.Is4() {
		return netip.Addr{}, fmt.Errorf("%w: route probe returned an unsupported local IP", ErrInvalidTCPGrant)
	}
	return address, nil
}

// StartTCP validates a private-host grant before creating a dynamic listener.
func StartTCP(ctx context.Context, grant TCPGrant) (*TCPRelay, error) {
	if grant.ListenerPort != 0 || grant.AllowRemotePeers {
		return nil, fmt.Errorf("%w: private relay requires a dynamic listener and exact owner peer", ErrInvalidTCPGrant)
	}
	listenConfig := net.ListenConfig{}
	return startTCP(ctx, grant, tcpDependencies{
		routeSource: RouteSourceAddr,
		listen:      listenConfig.Listen,
		dial:        (&net.Dialer{}).DialContext,
	})
}

// startLeaseManagedTCP creates a relay whose bounded lifetime is controlled by
// the authenticated lease helper rather than an independent one-shot timer.
func startLeaseManagedTCP(ctx context.Context, grant TCPGrant) (*TCPRelay, error) {
	if grant.ListenerPort != 0 || grant.AllowRemotePeers {
		return nil, fmt.Errorf("%w: private relay requires a dynamic listener and exact owner peer", ErrInvalidTCPGrant)
	}
	listenConfig := net.ListenConfig{}
	return startTCPWithLifetime(ctx, grant, tcpDependencies{
		routeSource: RouteSourceAddr,
		listen:      listenConfig.Listen,
		dial:        (&net.Dialer{}).DialContext,
	}, false)
}

// StartPublicationTCP is the dedicated host-publication path. The listener is
// exact and remote peers are accepted only for an explicitly exposed
// non-loopback bind. The destination remains one literal private workspace.
func StartPublicationTCP(ctx context.Context, grant TCPGrant) (*TCPRelay, error) {
	listenConfig := net.ListenConfig{}
	return startTCP(ctx, grant, tcpDependencies{
		allowLoopback: true,
		publication:   true,
		listen:        listenConfig.Listen,
		dial:          (&net.Dialer{}).DialContext,
	})
}

func startPublicationTCPWithDial(ctx context.Context, grant TCPGrant, dial func(context.Context, string, string) (net.Conn, error)) (*TCPRelay, error) {
	listenConfig := net.ListenConfig{}
	return startTCP(ctx, grant, tcpDependencies{
		allowLoopback: true,
		publication:   true,
		listen:        listenConfig.Listen,
		dial:          dial,
	})
}

func startLeaseManagedPublicationTCPWithDial(ctx context.Context, grant TCPGrant, dial func(context.Context, string, string) (net.Conn, error)) (*TCPRelay, error) {
	listenConfig := net.ListenConfig{}
	return startTCPWithLifetime(ctx, grant, tcpDependencies{
		allowLoopback: true,
		publication:   true,
		listen:        listenConfig.Listen,
		dial:          dial,
	}, false)
}

func startTCP(ctx context.Context, grant TCPGrant, deps tcpDependencies) (*TCPRelay, error) {
	return startTCPWithLifetime(ctx, grant, deps, true)
}

func startTCPWithLifetime(ctx context.Context, grant TCPGrant, deps tcpDependencies, oneShot bool) (*TCPRelay, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidTCPGrant)
	}
	grant.ListenerIP = grant.ListenerIP.Unmap()
	grant.OwnerIP = grant.OwnerIP.Unmap()
	if grant.Destination.IsValid() {
		grant.Destination = netip.AddrPortFrom(grant.Destination.Addr().Unmap(), grant.Destination.Port())
	}
	var validationErr error
	if deps.publication {
		validationErr = validatePublicationGrant(grant)
	} else {
		validationErr = validateGrant(grant, deps.allowLoopback)
	}
	if validationErr != nil {
		return nil, validationErr
	}
	if deps.listen == nil || deps.dial == nil || (!deps.publication && deps.routeSource == nil) {
		return nil, fmt.Errorf("%w: TCP relay dependency is unavailable", ErrInvalidTCPGrant)
	}

	if !deps.publication {
		routeIP, err := deps.routeSource(ctx, grant.OwnerIP)
		if err != nil {
			return nil, err
		}
		routeIP = routeIP.Unmap()
		if routeIP != grant.ListenerIP {
			return nil, fmt.Errorf("%w: listener IP %s is not the exact route source %s for owner %s", ErrInvalidTCPGrant, grant.ListenerIP, routeIP, grant.OwnerIP)
		}
	}

	var relayCtx context.Context
	var cancel context.CancelFunc
	if oneShot {
		relayCtx, cancel = context.WithTimeout(ctx, grant.Lease)
	} else {
		relayCtx, cancel = context.WithCancel(ctx)
	}
	listenerAddress := netip.AddrPortFrom(grant.ListenerIP, grant.ListenerPort)
	listenerNetwork := "tcp6"
	if grant.ListenerIP.Is4() {
		listenerNetwork = "tcp4"
	}
	listener, err := deps.listen(relayCtx, listenerNetwork, listenerAddress.String())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("listen on approved TCP address %s: %w", listenerAddress, err)
	}
	address, err := listenerAddrPort(listener.Addr())
	if err != nil {
		_ = listener.Close()
		cancel()
		return nil, err
	}
	address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	if address.Addr() != grant.ListenerIP || address.Port() == 0 || (grant.ListenerPort != 0 && address.Port() != grant.ListenerPort) {
		_ = listener.Close()
		cancel()
		return nil, fmt.Errorf("%w: listener bound unexpected address %s", ErrInvalidTCPGrant, address)
	}
	if address == grant.Destination {
		_ = listener.Close()
		cancel()
		return nil, fmt.Errorf("%w: destination would relay back to the listener", ErrInvalidTCPGrant)
	}

	relay := &TCPRelay{
		listener:    listener,
		address:     address,
		destination: grant.Destination,
		cancel:      cancel,
		dial:        deps.dial,
		conns:       make(map[net.Conn]struct{}),
		acceptDone:  make(chan struct{}),
		done:        make(chan struct{}),
	}
	go relay.accept(relayCtx, grant.OwnerIP, grant.AllowRemotePeers, deps.publication)
	go func() {
		<-relayCtx.Done()
		relay.finish()
	}()
	return relay, nil
}

func validatePublicationGrant(grant TCPGrant) error {
	loopback := netip.MustParseAddr("127.0.0.1")
	destination := grant.Destination.Addr().Unmap()
	if grant.Lease <= 0 || grant.Lease > MaxTCPLease {
		return fmt.Errorf("%w: lease must be positive and no longer than %s", ErrInvalidTCPGrant, MaxTCPLease)
	}
	if !grant.ListenerIP.IsValid() || grant.ListenerIP.Zone() != "" || grant.ListenerIP.IsMulticast() || grant.ListenerIP.IsLinkLocalUnicast() || grant.ListenerPort == 0 {
		return fmt.Errorf("%w: publication listener must be an exact address and port", ErrInvalidTCPGrant)
	}
	if grant.OwnerIP != loopback || !grant.DestinationLiteral {
		return fmt.Errorf("%w: publication requires loopback owner policy and literal destination", ErrInvalidTCPGrant)
	}
	if !grant.Destination.IsValid() || grant.Destination.Port() == 0 || !destination.Is4() || destination.IsLoopback() || (!destination.IsPrivate() && !sharedIPv4.Contains(destination)) {
		return fmt.Errorf("%w: publication destination must be a private IPv4 workspace address", ErrInvalidTCPGrant)
	}
	if grant.ListenerIP.IsLoopback() == grant.AllowRemotePeers {
		return fmt.Errorf("%w: publication remote-peer policy does not match the listener exposure", ErrInvalidTCPGrant)
	}
	return nil
}

func validateGrant(grant TCPGrant, allowLoopback bool) error {
	if grant.Lease <= 0 || grant.Lease > MaxTCPLease {
		return fmt.Errorf("%w: lease must be positive and no longer than %s", ErrInvalidTCPGrant, MaxTCPLease)
	}
	if err := validateHostAddress("listener", grant.ListenerIP, allowLoopback); err != nil {
		return err
	}
	if err := validateOwner(grant.OwnerIP, allowLoopback); err != nil {
		return err
	}
	if !grant.Destination.IsValid() || grant.Destination.Port() == 0 {
		return fmt.Errorf("%w: destination and port must be valid", ErrInvalidTCPGrant)
	}
	if err := validateDestination(grant.Destination.Addr(), allowLoopback); err != nil {
		return err
	}
	if grant.ListenerIP.Is4() != grant.OwnerIP.Is4() || grant.ListenerIP.Is4() != grant.Destination.Addr().Is4() {
		return fmt.Errorf("%w: listener, owner, and destination address families must match", ErrInvalidTCPGrant)
	}
	return nil
}

func validateHostAddress(label string, address netip.Addr, allowLoopback bool) error {
	if !address.IsValid() || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || (!address.IsGlobalUnicast() && !(allowLoopback && address.IsLoopback())) {
		return fmt.Errorf("%w: %s IP is unsupported", ErrInvalidTCPGrant, label)
	}
	if address.IsLoopback() && !allowLoopback {
		return fmt.Errorf("%w: %s IP must not be loopback", ErrInvalidTCPGrant, label)
	}
	return nil
}

func validateOwner(address netip.Addr, allowLoopback bool) error {
	if err := validateHostAddress("owner workspace", address, allowLoopback); err != nil {
		return err
	}
	if !allowLoopback || !address.IsLoopback() {
		if !address.IsPrivate() && !(address.Is4() && sharedIPv4.Contains(address)) {
			return fmt.Errorf("%w: owner workspace IP must be private", ErrInvalidTCPGrant)
		}
	}
	return nil
}

func validateDestination(address netip.Addr, allowLoopback bool) error {
	address = address.Unmap()
	if !address.IsValid() || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return fmt.Errorf("%w: destination IP is unsupported", ErrInvalidTCPGrant)
	}
	if address.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("%w: loopback destination is forbidden", ErrInvalidTCPGrant)
	}
	if !address.IsPrivate() && !(address.Is4() && sharedIPv4.Contains(address)) {
		return fmt.Errorf("%w: destination IP must be private", ErrInvalidTCPGrant)
	}
	return nil
}

func listenerAddrPort(address net.Addr) (netip.AddrPort, error) {
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("%w: listener returned %T, not TCP", ErrInvalidTCPGrant, address)
	}
	result := tcp.AddrPort()
	if !result.IsValid() {
		return netip.AddrPort{}, fmt.Errorf("%w: listener returned invalid TCP address", ErrInvalidTCPGrant)
	}
	return result, nil
}

func (r *TCPRelay) accept(ctx context.Context, ownerIP netip.Addr, allowRemotePeers, loopbackPeers bool) {
	defer close(r.acceptDone)
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if !r.isStopping() {
				r.setErr(fmt.Errorf("accept TCP relay connection: %w", err))
				r.cancel()
			}
			return
		}
		r.accepted.Add(1)
		if !r.addConn(conn) {
			_ = conn.Close()
			return
		}
		r.handlers.Add(1)
		go r.handle(ctx, conn, ownerIP, allowRemotePeers, loopbackPeers)
	}
}

func (r *TCPRelay) handle(ctx context.Context, inbound net.Conn, ownerIP netip.Addr, allowRemotePeers, loopbackPeers bool) {
	defer r.handlers.Done()
	defer r.removeAndClose(inbound)

	remote, ok := inbound.RemoteAddr().(*net.TCPAddr)
	remoteIP := netip.Addr{}
	if ok {
		remoteIP = remote.AddrPort().Addr().Unmap()
	}
	if !ok || (!allowRemotePeers && ((!loopbackPeers && remoteIP != ownerIP) || (loopbackPeers && !remoteIP.IsLoopback()))) {
		r.denied.Add(1)
		return
	}

	outbound, err := r.dial(ctx, "tcp", r.destination.String())
	if err != nil {
		r.dialFailures.Add(1)
		return
	}
	if !r.addConn(outbound) {
		_ = outbound.Close()
		return
	}
	defer r.removeAndClose(outbound)

	r.active.Add(1)
	defer r.active.Add(^uint64(0))
	pumpTCP(inbound, outbound)
	r.completed.Add(1)
}

func pumpTCP(left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if closer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		if closer, ok := source.(interface{ CloseRead() error }); ok {
			_ = closer.CloseRead()
		}
		done <- struct{}{}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	<-done
	<-done
}

func (r *TCPRelay) addConn(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *TCPRelay) removeAndClose(conn net.Conn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
	_ = conn.Close()
}

func (r *TCPRelay) isStopping() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopping
}

func (r *TCPRelay) setErr(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (r *TCPRelay) finish() {
	r.finishOnce.Do(func() {
		r.mu.Lock()
		r.stopping = true
		r.mu.Unlock()
		r.cancel()
		_ = r.listener.Close()
		<-r.acceptDone

		r.mu.Lock()
		connections := make([]net.Conn, 0, len(r.conns))
		for conn := range r.conns {
			connections = append(connections, conn)
		}
		r.mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		r.handlers.Wait()
		close(r.done)
	})
}

// Addr returns the exact dynamically allocated listener address.
func (r *TCPRelay) Addr() netip.AddrPort { return r.address }

// Destination returns the immutable pinned destination.
func (r *TCPRelay) Destination() netip.AddrPort { return r.destination }

// Done closes after the listener and all active connections have closed.
func (r *TCPRelay) Done() <-chan struct{} { return r.done }

// Err reports an unexpected listener failure, if any.
func (r *TCPRelay) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Counters reports connection lifecycle counts without payload information.
func (r *TCPRelay) Counters() TCPCounters {
	return TCPCounters{
		Accepted:     r.accepted.Load(),
		Denied:       r.denied.Load(),
		DialFailures: r.dialFailures.Load(),
		Active:       r.active.Load(),
		Completed:    r.completed.Load(),
	}
}

// Close synchronously closes the listener and every active connection. It
// reports any unexpected accept-loop failure recorded while the relay ran.
func (r *TCPRelay) Close() error {
	r.finish()
	return r.Err()
}
