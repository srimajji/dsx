package guest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
)

func TestHTTPAndTCPReadiness(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := probe(ctx, &guestproto.HealthSpec{Kind: "http", Target: server.URL}, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			t.Fatalf("HTTP probe = %v", err)
		}
	})
	t.Run("tcp", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		accepted := make(chan struct{})
		go func() {
			connection, acceptErr := listener.Accept()
			if acceptErr == nil {
				_ = connection.Close()
			}
			close(accepted)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := probe(ctx, &guestproto.HealthSpec{Kind: "tcp", Target: listener.Addr().String()}, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
			t.Fatalf("TCP probe = %v", err)
		}
		<-accepted
	})
}

func TestReadinessTargetsMustBeLoopback(t *testing.T) {
	params := guestproto.StartParams{Processes: []guestproto.ProcessSpec{{ID: "external", Health: &guestproto.HealthSpec{Kind: "tcp", Target: "0.0.0.0:80"}}}}
	if err := validateHealthTargets(params); err == nil {
		t.Fatal("non-loopback TCP readiness was accepted")
	}
	params.Processes[0].Health = &guestproto.HealthSpec{Kind: "http", Target: "http://example.com/health"}
	if err := validateHealthTargets(params); err == nil {
		t.Fatal("non-loopback HTTP readiness was accepted")
	}
}
