package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"time"
)

// RunLeappMirrorCommand dispatches the single hidden, pipe-only Leapp mirror
// entry point. Start and authenticated control requests share this command so
// no credential-bearing value can enter argv or environment.
func RunLeappMirrorCommand() int {
	stdinInfo, err := os.Stdin.Stat()
	if err != nil || stdinInfo.Mode()&os.ModeNamedPipe == 0 || stdinInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	var command leappMirrorCommand
	if err := decodeBoundedJSON(os.Stdin, MaxHelperInputBytes, &command); err != nil || command.Version != 1 {
		return 2
	}
	if command.Action == "start" {
		return runLeappMirrorStart(command)
	}
	return runLeappMirrorControl(command)
}

func runLeappMirrorControl(request leappMirrorCommand) int {
	stdoutInfo, err := os.Stdout.Stat()
	if err != nil || stdoutInfo.Mode()&os.ModeNamedPipe == 0 || stdoutInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	if request.Identity.Validate() != nil || request.SpecDigest == "" || (request.Action != "status" && request.Action != "stop") {
		return 2
	}
	decodedToken, err := base64.RawURLEncoding.DecodeString(request.Token)
	if err != nil || len(decodedToken) != 32 {
		return 2
	}
	workingDirectory, err := os.Getwd()
	if err != nil || verifyPrivateDirectory(workingDirectory) != nil || verifyPrivateSocket(leappMirrorSocketName) != nil {
		return 2
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.Dial("unix", leappMirrorSocketName)
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
	var response leappMirrorResponse
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
