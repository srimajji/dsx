package guest

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestRelayLoopbackCopiesBidirectionalStream(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		request, readErr := io.ReadAll(connection)
		if readErr == nil {
			_, readErr = connection.Write(append([]byte("reply:"), request...))
		}
		serverDone <- readErr
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	var output bytes.Buffer
	if err := RelayLoopback(bytes.NewBufferString("request"), &output, port); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if output.String() != "reply:request" {
		t.Fatalf("relay output = %q", output.String())
	}
}
