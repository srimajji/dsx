package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"time"
)

// RunLeaseControlClient is a short-lived hidden transport used so the Unix
// socket can have a relative sockaddr while its inode remains in the deep,
// private workspace lease directory. Tokens and requests travel only by pipe.
func RunLeaseControlClient() int {
	stdinInfo, err := os.Stdin.Stat()
	if err != nil || stdinInfo.Mode()&os.ModeNamedPipe == 0 || stdinInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	stdoutInfo, err := os.Stdout.Stat()
	if err != nil || stdoutInfo.Mode()&os.ModeNamedPipe == 0 || stdoutInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	workingDirectory, err := os.Getwd()
	if err != nil || verifyPrivateDirectory(workingDirectory) != nil || verifyPrivateSocket(controlSocketName) != nil {
		return 2
	}
	var request controlRequest
	if err := decodeBoundedJSON(os.Stdin, MaxControlBytes, &request); err != nil || request.Version != 1 || request.Identity.Validate() != nil || request.SpecDigest == "" || (request.Operation != "status" && request.Operation != "renew" && request.Operation != "stop") {
		return 2
	}
	decodedToken, err := base64.RawURLEncoding.DecodeString(request.Token)
	if err != nil || len(decodedToken) != 32 {
		return 2
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.Dial("unix", controlSocketName)
	if err != nil {
		return 1
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(defaultStopWait)); err != nil {
		return 1
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxControlBytes {
		return 2
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return 1
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok || unixConnection.CloseWrite() != nil {
		return 1
	}
	var response controlResponse
	if err := decodeBoundedJSON(connection, MaxControlBytes, &response); err != nil {
		return 1
	}
	output, err := json.Marshal(response)
	if err != nil || len(output) > MaxControlBytes {
		return 1
	}
	output = append(output, '\n')
	written, err := os.Stdout.Write(output)
	if err != nil || written != len(output) {
		return 1
	}
	return 0
}
