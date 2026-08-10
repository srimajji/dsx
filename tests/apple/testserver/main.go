package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	listener, err := net.Listen("tcp", ":3000")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("/workspace/dsx-http-listening", []byte("listening\n"), 0o600); err != nil {
		panic(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("/workspace/dsx-http-accepted", []byte("accepted\n"), 0o600); err != nil {
		panic(err)
	}
	reader := bufio.NewReader(connection)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			panic(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := fmt.Fprint(connection, "HTTP/1.1 200 OK\r\nContent-Length: 21\r\nConnection: close\r\n\r\ndsx-apple-fixed-port\n"); err != nil {
		panic(err)
	}
	if err := connection.Close(); err != nil {
		panic(err)
	}
	if err := listener.Close(); err != nil {
		panic(err)
	}
}
