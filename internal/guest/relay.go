package guest

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// RelayLoopback connects one inherited byte stream to a guest-loopback TCP
// listener. The destination host is deliberately not configurable.
func RelayLoopback(stdin io.Reader, stdout io.Writer, port uint16) error {
	if stdin == nil || stdout == nil || port == 0 {
		return fmt.Errorf("invalid loopback relay request")
	}
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect guest loopback port: %w", err)
	}
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		_ = connection.Close()
		return fmt.Errorf("guest loopback connection is not TCP")
	}
	defer tcp.Close()
	go func() {
		_, _ = io.Copy(tcp, stdin)
		_ = tcp.CloseWrite()
	}()
	if _, err := io.Copy(stdout, tcp); err != nil {
		return fmt.Errorf("copy guest loopback response: %w", err)
	}
	_ = tcp.CloseRead()
	return nil
}
