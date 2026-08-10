package bridge

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

type fuzzTCPResolver struct {
	answers []netip.Addr
	calls   int
}

func (resolver *fuzzTCPResolver) LookupNetIP(_ context.Context, network, _ string) ([]netip.Addr, error) {
	resolver.calls++
	if network != "ip" {
		return nil, ErrInvalidTCPGrant
	}
	return append([]netip.Addr(nil), resolver.answers...), nil
}

func FuzzTCPDestinationValidation(f *testing.F) {
	for _, seed := range []struct {
		host          string
		port          int
		first, second string
	}{
		{"10.20.30.40", 443, "", ""},
		{"100.64.0.1", 443, "", ""},
		{"8.8.8.8", 443, "", ""},
		{"203.0.113.10", 443, "", ""},
		{"127.0.0.1", 8080, "", ""},
		{"::ffff:203.0.113.10", 443, "", ""},
		{"destination.example", 443, "203.0.113.10", ""},
		{"destination.example", 443, "203.0.113.10", "203.0.113.11"},
		{"destination.example", 443, "192.168.64.20", ""},
		{"destination.example", 443, "100.100.10.20", ""},
		{"destination.example", 443, "8.8.8.8", ""},
		{"destination.example", 443, "127.0.0.1", ""},
		{" destination.example", 443, "203.0.113.10", ""},
		{"destination.example", 0, "203.0.113.10", ""},
		{"destination.example", 65536, "203.0.113.10", ""},
		{"destination\x00.example", 443, "203.0.113.10", ""},
		{string([]byte{0xff, 0xfe}), 443, "203.0.113.10", ""},
	} {
		f.Add(seed.host, seed.port, seed.first, seed.second)
	}

	f.Fuzz(func(t *testing.T, host string, port int, first, second string) {
		if len(host) > 256 || len(first) > 128 || len(second) > 128 {
			t.Skip()
		}
		answers := parseFuzzAddresses(first, second)
		resolver := &fuzzTCPResolver{answers: answers}
		got, err := ResolveTCPDestination(context.Background(), resolver, host, port)

		wantValid := host != "" && strings.TrimSpace(host) == host && port >= 1 && port <= 65535
		literal, literalErr := netip.ParseAddr(host)
		if literalErr == nil {
			literal = literal.Unmap()
			wantValid = wantValid && validateDestination(literal, false) == nil
			if resolver.calls != 0 {
				t.Fatalf("literal destination %q unexpectedly used DNS", host)
			}
		} else {
			wantValid = wantValid && len(answers) == 1 && validateDestination(singleAddress(answers), false) == nil
			if wantValid && resolver.calls != 1 {
				t.Fatalf("DNS destination %q lookup count = %d, want 1", host, resolver.calls)
			}
		}
		if (err == nil) != wantValid {
			t.Fatalf("ResolveTCPDestination(%q, %d, %v) = %v, %v; validity=%t", host, port, answers, got, err, wantValid)
		}
		if err != nil {
			return
		}
		if !got.IsValid() || got.Port() != uint16(port) {
			t.Fatalf("resolved destination lost exact port/address: %v", got)
		}
		if err := RevalidateTCPDestination(context.Background(), &fuzzTCPResolver{answers: answers}, host, got); err != nil {
			t.Fatalf("unchanged destination failed revalidation: %v", err)
		}
		changed := netip.MustParseAddrPort("192.0.2.1:1")
		if got.Addr() == changed.Addr() {
			changed = netip.MustParseAddrPort("192.0.2.2:1")
		}
		changed = netip.AddrPortFrom(changed.Addr(), got.Port())
		if err := RevalidateTCPDestination(context.Background(), &fuzzTCPResolver{answers: answers}, host, changed); err == nil {
			t.Fatalf("destination change from %v to %v was accepted", changed, got)
		}
	})
}

func parseFuzzAddresses(values ...string) []netip.Addr {
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if address, err := netip.ParseAddr(value); err == nil {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

func singleAddress(addresses []netip.Addr) netip.Addr {
	if len(addresses) != 1 {
		return netip.Addr{}
	}
	return addresses[0]
}
