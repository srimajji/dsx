package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type controlledTCPListener struct {
	address net.Addr
	accept  chan error
	closed  chan struct{}
	once    sync.Once
}

func (listener *controlledTCPListener) Accept() (net.Conn, error) {
	select {
	case err := <-listener.accept:
		return nil, err
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}

func (listener *controlledTCPListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (listener *controlledTCPListener) Addr() net.Addr {
	return listener.address
}

func loopbackRelay(t *testing.T, ctx context.Context, destination netip.AddrPort, lease time.Duration) *TCPRelay {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	listenConfig := net.ListenConfig{}
	relay, err := startTCP(ctx, TCPGrant{
		ListenerIP:         loopback,
		OwnerIP:            loopback,
		Destination:        destination,
		DestinationLiteral: true,
		Lease:              lease,
	}, tcpDependencies{
		allowLoopback: true,
		routeSource: func(context.Context, netip.Addr) (netip.Addr, error) {
			return loopback, nil
		},
		listen: listenConfig.Listen,
		dial:   (&net.Dialer{}).DialContext,
	})
	if err != nil {
		t.Fatalf("start loopback test relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	return relay
}

func listenLoopback(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func addrPort(t *testing.T, listener *net.TCPListener) netip.AddrPort {
	t.Helper()
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	if !address.IsValid() || address.Port() == 0 {
		t.Fatalf("invalid listener address %v", address)
	}
	return netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relay closure")
	}
}

func TestPrivateRelayOwnerPassesBytesWithoutCONNECTInterpretation(t *testing.T) {
	destination := listenLoopback(t)
	received := make(chan []byte, 1)
	go func() {
		conn, err := destination.AcceptTCP()
		if err != nil {
			return
		}
		defer conn.Close()
		payload, err := io.ReadAll(conn)
		if err != nil {
			return
		}
		received <- payload
		_, _ = conn.Write(payload)
	}()

	relay := loopbackRelay(t, context.Background(), addrPort(t, destination), time.Minute)
	client, err := net.DialTCP("tcp4", nil, net.TCPAddrFromAddrPort(relay.Addr()))
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	payload := []byte("CONNECT forbidden.example:9443 HTTP/1.1\r\nHost: ignored\r\n\r\nopaque-body")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write relay payload: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close relay client: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read relayed payload: %v", err)
	}
	_ = client.Close()
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("relay changed payload: got %q want %q", got, payload)
	}
	select {
	case atDestination := <-received:
		if !reflect.DeepEqual(atDestination, payload) {
			t.Fatalf("destination got %q want exact %q", atDestination, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("destination did not receive owner payload")
	}
	if relay.Destination() != addrPort(t, destination) {
		t.Fatalf("destination changed: got %s", relay.Destination())
	}
	_ = relay.Close()
	counters := relay.Counters()
	if counters.Accepted != 1 || counters.Denied != 0 || counters.Active != 0 || counters.Completed != 1 {
		t.Fatalf("unexpected counters: %+v", counters)
	}
}

func TestPrivateRelayDeniesCrossSourceBeforeDial(t *testing.T) {
	destination := listenLoopback(t)
	accepted := make(chan struct{}, 1)
	_ = destination.SetDeadline(time.Now().Add(400 * time.Millisecond))
	go func() {
		conn, err := destination.AcceptTCP()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	relay := loopbackRelay(t, context.Background(), addrPort(t, destination), time.Minute)
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}
	conn, err := dialer.DialContext(context.Background(), "tcp4", relay.Addr().String())
	if err != nil {
		t.Skipf("platform cannot select a distinct loopback source: %v", err)
	}
	_, _ = conn.Write([]byte("must not reach destination"))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("cross-source connection remained usable")
	}
	_ = conn.Close()

	select {
	case <-accepted:
		t.Fatal("relay dialed destination for denied source")
	case <-time.After(500 * time.Millisecond):
	}
	_ = relay.Close()
	counters := relay.Counters()
	if counters.Accepted != 1 || counters.Denied != 1 || counters.DialFailures != 0 || counters.Completed != 0 {
		t.Fatalf("unexpected denial counters: %+v", counters)
	}
}

func TestPrivateRelayDestinationAndPortAreConfined(t *testing.T) {
	approved := listenLoopback(t)
	alternate := listenLoopback(t)
	alternateAccepted := make(chan struct{}, 1)
	_ = alternate.SetDeadline(time.Now().Add(400 * time.Millisecond))
	go func() {
		conn, err := alternate.AcceptTCP()
		if err == nil {
			alternateAccepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	approvedPayload := make(chan []byte, 1)
	go func() {
		conn, err := approved.AcceptTCP()
		if err != nil {
			return
		}
		defer conn.Close()
		payload, _ := io.ReadAll(conn)
		approvedPayload <- payload
	}()

	relay := loopbackRelay(t, context.Background(), addrPort(t, approved), time.Minute)
	conn, err := net.Dial("tcp4", relay.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	attempt := []byte("CONNECT " + addrPort(t, alternate).String() + "\r\n")
	_, _ = conn.Write(attempt)
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	select {
	case got := <-approvedPayload:
		if !reflect.DeepEqual(got, attempt) {
			t.Fatalf("approved destination got %q want %q", got, attempt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approved destination was not reached")
	}
	_ = conn.Close()
	select {
	case <-alternateAccepted:
		t.Fatal("alternate destination or port was reached")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestResolveTCPDestinationPinsExactlyOneApprovedAnswer(t *testing.T) {
	one := netip.MustParseAddr("100.100.10.20")
	resolver := resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "service.internal" {
			t.Fatalf("unexpected lookup %q %q", network, host)
		}
		return []netip.Addr{one}, nil
	})
	got, err := ResolveTCPDestination(context.Background(), resolver, "service.internal", 8443)
	if err != nil {
		t.Fatalf("resolve exact CGNAT destination: %v", err)
	}
	if want := netip.AddrPortFrom(one, 8443); got != want {
		t.Fatalf("got %s want %s", got, want)
	}

	private, err := ResolveTCPDestination(context.Background(), nil, "10.20.30.40", 9443)
	if err != nil || private != netip.MustParseAddrPort("10.20.30.40:9443") {
		t.Fatalf("explicit private literal: got %s, %v", private, err)
	}

	for name, answers := range map[string][]netip.Addr{
		"empty":         nil,
		"multiple":      {one, netip.MustParseAddr("100.100.10.21")},
		"duplicates":    {one, one},
		"loopback":      {netip.MustParseAddr("127.0.0.1")},
		"public":        {netip.MustParseAddr("8.8.8.8")},
		"mapped public": {netip.MustParseAddr("::ffff:8.8.8.8")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveTCPDestination(context.Background(), resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				return answers, nil
			}), "service.internal", 443)
			if !errors.Is(err, ErrInvalidTCPGrant) {
				t.Fatalf("got %v, want invalid grant", err)
			}
		})
	}

	for _, host := range []string{"8.8.8.8", "::ffff:8.8.8.8"} {
		if _, err := ResolveTCPDestination(context.Background(), nil, host, 443); !errors.Is(err, ErrInvalidTCPGrant) {
			t.Fatalf("public literal %q got %v, want invalid grant", host, err)
		}
	}
	if _, err := ResolveTCPDestination(context.Background(), nil, "127.0.0.1", 443); !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("loopback literal got %v, want invalid grant", err)
	}
	for _, port := range []int{-1, 0, 65536} {
		if _, err := ResolveTCPDestination(context.Background(), resolver, "service.internal", port); !errors.Is(err, ErrInvalidTCPGrant) {
			t.Fatalf("port %d: got %v, want invalid grant", port, err)
		}
	}
}

func TestRevalidateTCPDestinationRejectsChangedAnswer(t *testing.T) {
	expected := netip.MustParseAddrPort("100.100.10.20:443")

	t.Run("different approved address", func(t *testing.T) {
		changed := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("100.100.10.21")}, nil
		})
		if err := RevalidateTCPDestination(context.Background(), changed, "service.internal", expected); !errors.Is(err, ErrTCPDestinationChanged) {
			t.Fatalf("got %v, want destination-changed error", err)
		}
	})

	t.Run("public address", func(t *testing.T) {
		public := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		})
		if err := RevalidateTCPDestination(context.Background(), public, "service.internal", expected); !errors.Is(err, ErrInvalidTCPGrant) {
			t.Fatalf("got %v, want public destination rejection", err)
		}
	})
}

func TestPrivateRelayCloseReportsUnexpectedAcceptFailure(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	listener := &controlledTCPListener{
		address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 43210},
		accept:  make(chan error, 1),
		closed:  make(chan struct{}),
	}
	relay, err := startTCP(context.Background(), TCPGrant{
		ListenerIP:         loopback,
		OwnerIP:            loopback,
		Destination:        netip.MustParseAddrPort("127.0.0.1:443"),
		DestinationLiteral: true,
		Lease:              time.Minute,
	}, tcpDependencies{
		allowLoopback: true,
		routeSource:   func(context.Context, netip.Addr) (netip.Addr, error) { return loopback, nil },
		listen:        func(context.Context, string, string) (net.Listener, error) { return listener, nil },
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("unexpected dial")
		},
	})
	if err != nil {
		t.Fatalf("start controlled relay: %v", err)
	}

	acceptErr := errors.New("injected accept failure")
	listener.accept <- acceptErr
	waitDone(t, relay.Done())
	if !errors.Is(relay.Err(), acceptErr) {
		t.Fatalf("relay Err() = %v, want injected accept failure", relay.Err())
	}
	for call := 1; call <= 2; call++ {
		if err := relay.Close(); !errors.Is(err, acceptErr) {
			t.Fatalf("Close() call %d = %v, want injected accept failure", call, err)
		}
	}
}

func TestRouteSourceAddressDerivationIsDeterministic(t *testing.T) {
	owner := netip.MustParseAddr("192.168.64.7")
	want := netip.MustParseAddr("192.168.64.1")
	calls := 0
	got, err := routeSourceAddr(context.Background(), owner, func(_ context.Context, network, address string) (net.Addr, error) {
		calls++
		if network != "udp4" || address != "192.168.64.7:9" {
			t.Fatalf("unexpected probe %q %q", network, address)
		}
		return &net.UDPAddr{IP: net.IPv4(192, 168, 64, 1), Port: 49152}, nil
	})
	if err != nil {
		t.Fatalf("derive route source: %v", err)
	}
	if got != want || calls != 1 {
		t.Fatalf("got %s after %d calls, want %s after one call", got, calls, want)
	}

	_, err = routeSourceAddr(context.Background(), netip.MustParseAddr("fd00::7"), func(context.Context, string, string) (net.Addr, error) {
		return &net.UDPAddr{IP: net.IPv4(192, 168, 64, 1)}, nil
	})
	if !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("family mismatch got %v, want invalid grant", err)
	}
}

func TestPrivateRelayRejectsUnsafeGrantBeforeListening(t *testing.T) {
	valid := TCPGrant{
		ListenerIP:  netip.MustParseAddr("192.168.64.1"),
		OwnerIP:     netip.MustParseAddr("192.168.64.7"),
		Destination: netip.MustParseAddrPort("100.100.10.20:443"),
		Lease:       time.Minute,
	}
	tests := map[string]TCPGrant{
		"wildcard listener": func() TCPGrant { g := valid; g.ListenerIP = netip.IPv4Unspecified(); return g }(),
		"loopback listener": func() TCPGrant { g := valid; g.ListenerIP = netip.MustParseAddr("127.0.0.1"); return g }(),
		"public owner":      func() TCPGrant { g := valid; g.OwnerIP = netip.MustParseAddr("8.8.8.8"); return g }(),
		"zero port":         func() TCPGrant { g := valid; g.Destination = netip.AddrPortFrom(g.Destination.Addr(), 0); return g }(),
		"public target":     func() TCPGrant { g := valid; g.Destination = netip.MustParseAddrPort("8.8.8.8:443"); return g }(),
		"zero lease":        func() TCPGrant { g := valid; g.Lease = 0; return g }(),
		"unbounded lease":   func() TCPGrant { g := valid; g.Lease = MaxTCPLease + time.Nanosecond; return g }(),
		"family mismatch":   func() TCPGrant { g := valid; g.Destination = netip.MustParseAddrPort("[fd00::20]:443"); return g }(),
		"multicast target":  func() TCPGrant { g := valid; g.Destination = netip.MustParseAddrPort("224.0.0.1:443"); return g }(),
		"implicit loopback": func() TCPGrant { g := valid; g.Destination = netip.MustParseAddrPort("127.0.0.1:443"); return g }(),
		"literal loopback": func() TCPGrant {
			g := valid
			g.Destination = netip.MustParseAddrPort("127.0.0.1:443")
			g.DestinationLiteral = true
			return g
		}(),
	}
	for name, grant := range tests {
		t.Run(name, func(t *testing.T) {
			listened := false
			dialed := false
			_, err := startTCP(context.Background(), grant, tcpDependencies{
				routeSource: func(context.Context, netip.Addr) (netip.Addr, error) { return grant.ListenerIP, nil },
				listen: func(context.Context, string, string) (net.Listener, error) {
					listened = true
					return nil, errors.New("unexpected listen")
				},
				dial: func(context.Context, string, string) (net.Conn, error) {
					dialed = true
					return nil, errors.New("unexpected dial")
				},
			})
			if !errors.Is(err, ErrInvalidTCPGrant) {
				t.Fatalf("got %v, want invalid grant", err)
			}
			if listened || dialed {
				t.Fatalf("unsafe grant reached listener or dial: listened=%t dialed=%t", listened, dialed)
			}
		})
	}
}

func TestPrivateRelayRejectsRouteInterfaceMismatchBeforeListening(t *testing.T) {
	grant := TCPGrant{
		ListenerIP:  netip.MustParseAddr("192.168.64.1"),
		OwnerIP:     netip.MustParseAddr("192.168.64.7"),
		Destination: netip.MustParseAddrPort("100.100.10.20:443"),
		Lease:       time.Minute,
	}
	for name, routeIP := range map[string]netip.Addr{
		"wifi":      netip.MustParseAddr("192.168.1.10"),
		"tailscale": netip.MustParseAddr("100.100.10.10"),
	} {
		t.Run(name, func(t *testing.T) {
			listened := false
			_, err := startTCP(context.Background(), grant, tcpDependencies{
				routeSource: func(context.Context, netip.Addr) (netip.Addr, error) { return routeIP, nil },
				listen: func(context.Context, string, string) (net.Listener, error) {
					listened = true
					return nil, errors.New("unexpected listen")
				},
				dial: (&net.Dialer{}).DialContext,
			})
			if !errors.Is(err, ErrInvalidTCPGrant) || listened {
				t.Fatalf("got err=%v listened=%v, want mismatch before listen", err, listened)
			}
		})
	}
}

func TestPrivateRelayRejectsSelfRelay(t *testing.T) {
	listener := listenLoopback(t)
	destination := addrPort(t, listener)
	loopback := netip.MustParseAddr("127.0.0.1")
	_, err := startTCP(context.Background(), TCPGrant{
		ListenerIP:         loopback,
		OwnerIP:            loopback,
		Destination:        destination,
		DestinationLiteral: true,
		Lease:              time.Minute,
	}, tcpDependencies{
		allowLoopback: true,
		routeSource:   func(context.Context, netip.Addr) (netip.Addr, error) { return loopback, nil },
		listen:        func(context.Context, string, string) (net.Listener, error) { return listener, nil },
		dial:          (&net.Dialer{}).DialContext,
	})
	if !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("got %v, want self-relay rejection", err)
	}
}

func TestPrivateRelayContextAndTTLClosure(t *testing.T) {
	destination := listenLoopback(t)

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		relay := loopbackRelay(t, ctx, addrPort(t, destination), time.Minute)
		cancel()
		waitDone(t, relay.Done())
		if err := relay.Close(); err != nil {
			t.Fatalf("close context-canceled relay: %v", err)
		}
		if err := relay.Err(); err != nil {
			t.Fatalf("context cancellation recorded unexpected error: %v", err)
		}
		if _, err := net.DialTimeout("tcp4", relay.Addr().String(), 100*time.Millisecond); err == nil {
			t.Fatal("context-canceled listener still accepted connections")
		}
	})

	t.Run("ttl", func(t *testing.T) {
		relay := loopbackRelay(t, context.Background(), addrPort(t, destination), 50*time.Millisecond)
		waitDone(t, relay.Done())
		if err := relay.Close(); err != nil {
			t.Fatalf("close expired relay: %v", err)
		}
		if err := relay.Err(); err != nil {
			t.Fatalf("lease expiration recorded unexpected error: %v", err)
		}
		if _, err := net.DialTimeout("tcp4", relay.Addr().String(), 100*time.Millisecond); err == nil {
			t.Fatal("expired listener still accepted connections")
		}
	})
}

func TestPrivateRelayCloseTerminatesActiveConnections(t *testing.T) {
	destination := listenLoopback(t)
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, err := destination.AcceptTCP()
		if err != nil {
			close(closed)
			return
		}
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		close(closed)
	}()

	relay := loopbackRelay(t, context.Background(), addrPort(t, destination), time.Minute)
	client, err := net.Dial("tcp4", relay.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	_, _ = client.Write([]byte("keep-open"))
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("destination did not accept active relay connection")
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("close relay: %v", err)
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("second close relay: %v", err)
	}
	if err := relay.Err(); err != nil {
		t.Fatalf("normal listener closure recorded unexpected error: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active client connection survived relay closure")
	}
	_ = client.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("destination connection survived relay closure")
	}
	if got := relay.Counters().Active; got != 0 {
		t.Fatalf("active count after closure = %d, want 0", got)
	}
}

func TestTCPCountersExposeLifecycleCountsOnly(t *testing.T) {
	typeOfCounters := reflect.TypeOf(TCPCounters{})
	want := []string{"Accepted", "Denied", "DialFailures", "Active", "Completed"}
	if typeOfCounters.NumField() != len(want) {
		t.Fatalf("TCPCounters has %d fields, want %d", typeOfCounters.NumField(), len(want))
	}
	for index, name := range want {
		field := typeOfCounters.Field(index)
		if field.Name != name || field.Type.Kind() != reflect.Uint64 {
			t.Fatalf("counter field %d = %s %s, want %s uint64", index, field.Name, field.Type, name)
		}
	}
}

func TestPublicationRelayRequiresExactExposurePolicy(t *testing.T) {
	base := TCPGrant{
		ListenerIP: netip.MustParseAddr("127.0.0.1"), ListenerPort: 49152,
		OwnerIP:            netip.MustParseAddr("127.0.0.1"),
		Destination:        netip.MustParseAddrPort("192.168.64.12:3000"),
		DestinationLiteral: true, Lease: time.Minute,
	}
	if err := validatePublicationGrant(base); err != nil {
		t.Fatalf("loopback-only publication rejected: %v", err)
	}
	remote := base
	remote.ListenerIP = netip.MustParseAddr("0.0.0.0")
	if err := validatePublicationGrant(remote); !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("ungranted nonloopback publication error = %v", err)
	}
	remote.AllowRemotePeers = true
	if err := validatePublicationGrant(remote); err != nil {
		t.Fatalf("explicit nonloopback publication rejected: %v", err)
	}
	unsafe := base
	unsafe.Destination = netip.MustParseAddrPort("8.8.8.8:3000")
	if err := validatePublicationGrant(unsafe); !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("public destination error = %v", err)
	}
	if _, err := StartTCP(context.Background(), base); !errors.Is(err, ErrInvalidTCPGrant) {
		t.Fatalf("private StartTCP accepted publication grant: %v", err)
	}
}

func TestExplicitWildcardPublicationBindsRequestedIPv4Address(t *testing.T) {
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(reservation.Addr().(*net.TCPAddr).Port)
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	relay, err := StartPublicationTCP(context.Background(), TCPGrant{
		ListenerIP: netip.MustParseAddr("0.0.0.0"), ListenerPort: port,
		OwnerIP: netip.MustParseAddr("127.0.0.1"), Destination: netip.MustParseAddrPort("192.168.64.12:3000"),
		DestinationLiteral: true, AllowRemotePeers: true, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	if relay.Addr().Addr() != netip.MustParseAddr("0.0.0.0") || relay.Addr().Port() != port {
		t.Fatalf("wildcard publication listener = %s", relay.Addr())
	}
}
