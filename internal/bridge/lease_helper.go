package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// RunLeaseHelper runs the hidden, pipe-only helper entry point. Callers must
// dispatch it before constructing the public CLI. It never accepts flags.
func RunLeaseHelper() int {
	ready := os.NewFile(uintptr(3), "bridge-ready")
	if ready == nil {
		return 2
	}
	defer ready.Close()
	stdinInfo, err := os.Stdin.Stat()
	if err != nil || stdinInfo.Mode()&os.ModeNamedPipe == 0 || stdinInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	readyInfo, err := ready.Stat()
	if err != nil || readyInfo.Mode()&os.ModeNamedPipe == 0 || readyInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	var envelope helperEnvelope
	if err := decodeBoundedJSON(os.Stdin, MaxHelperInputBytes, &envelope); err != nil {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Failure: "startup_failed"})
		return 2
	}
	_ = os.Stdin.Close()
	_ = os.Stdout.Close()
	_ = os.Stderr.Close()
	if envelope.Version != 1 || envelope.StateRoot == "" || envelope.SpecDigest == "" || envelope.Token == "" || envelope.Identity.Validate() != nil {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: envelope.SpecDigest, Failure: "startup_failed"})
		return 2
	}
	specs, digest, err := validateRelaySpecs(envelope.Specs)
	if err != nil || digest != envelope.SpecDigest {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: envelope.SpecDigest, Failure: "startup_failed"})
		return 2
	}
	executablePath, err := os.Executable()
	if err != nil {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: digest, Failure: "startup_failed"})
		return 2
	}
	executable, err := canonicalExecutableIdentity(executablePath)
	if err != nil || executable != envelope.Executable {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: digest, Failure: "startup_failed"})
		return 2
	}
	if relaySpecsRequireContainer(specs) {
		currentContainer, containerErr := canonicalExecutableIdentity(envelope.ContainerExecutable.Path)
		if containerErr != nil || currentContainer != envelope.ContainerExecutable {
			writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: digest, Executable: executable, Failure: "startup_failed"})
			return 2
		}
	}
	paths, err := verifiedHelperPaths(envelope.StateRoot, envelope.Identity)
	if err != nil {
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: digest, Executable: executable, Failure: "startup_failed"})
		return 2
	}
	token, err := readPrivateToken(paths.token)
	if err != nil || !constantTimeTokenEqual(token, envelope.Token) {
		writeFailure(paths, envelope.Identity, digest, "startup_failed")
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: envelope.Identity, SpecDigest: digest, Executable: executable, Failure: "startup_failed"})
		return 2
	}
	return runVerifiedLeaseHelper(ready, paths, envelope.Identity, specs, digest, token, executable, envelope.ContainerExecutable)
}

func runVerifiedLeaseHelper(ready io.Writer, paths leasePaths, identity LeaseIdentity, specs []RelaySpec, digest, token string, executable, containerExecutable executableIdentity) int {
	startedAt := time.Now().UTC()
	relays := make([]*TCPRelay, 0, len(specs))
	result := LeaseResult{Bindings: make([]ListenerBinding, 0, len(specs)), Environment: make(map[string]string)}
	fail := func(code string) int {
		cleanupHelperArtifacts(paths, relays, false)
		writeFailure(paths, identity, digest, code)
		writeHelperReady(ready, helperReady{Version: 1, State: "error", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable, Failure: code})
		return 1
	}
	for _, spec := range specs {
		grant := TCPGrant{
			ListenerIP: spec.ListenerIP, ListenerPort: spec.ListenerPort, OwnerIP: spec.OwnerIP,
			Destination: spec.Destination, DestinationLiteral: spec.DestinationLiteral,
			AllowRemotePeers: spec.AllowRemotePeers, Lease: spec.Lease,
		}
		var relay *TCPRelay
		var err error
		switch spec.Mode {
		case RelayModePrivateHost:
			relay, err = startLeaseManagedTCP(context.Background(), grant)
		case RelayModePublication:
			relay, err = startLeaseManagedPublicationTCPWithDial(context.Background(), grant, func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialOwnedWorkspaceLoopback(ctx, containerExecutable, identity, *spec.Publication, spec.Destination.Port())
			})
		default:
			err = errors.New("invalid relay mode")
		}
		if err != nil {
			return fail("startup_failed")
		}
		relays = append(relays, relay)
		address := relay.Addr()
		result.Bindings = append(result.Bindings, ListenerBinding{Name: spec.Name, Mode: spec.Mode, Addr: address.Addr(), Port: address.Port()})
		if spec.Mode == RelayModePrivateHost {
			base, _ := relayEnvironmentBase(spec.Name)
			result.Environment[base+"_HOST"] = address.Addr().String()
			result.Environment[base+"_PORT"] = strconv.Itoa(int(address.Port()))
		}
	}
	if !validLeaseResult(specs, result) {
		return fail("startup_failed")
	}
	if _, err := os.Lstat(paths.socket); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fail("startup_failed")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlSocketName, Net: "unix"})
	if err != nil {
		return fail("startup_failed")
	}
	if err := os.Chmod(paths.socket, 0o600); err != nil || verifyPrivateSocket(paths.socket) != nil {
		listener.Close()
		return fail("startup_failed")
	}
	ledger := leaseLedger{Version: 1, Identity: identity, SpecDigest: digest, PID: os.Getpid(), ProcessStartedAt: startedAt, Executable: executable, Result: cloneLeaseResult(result)}
	encoded, err := json.Marshal(ledger)
	if err != nil || len(encoded) > MaxControlBytes || atomicWritePrivate(paths.ledger, encoded) != nil {
		listener.Close()
		return fail("startup_failed")
	}
	_ = os.Remove(paths.failure)
	if !writeHelperReady(ready, helperReady{Version: 1, State: "ready", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable, Result: result}) {
		listener.Close()
		cleanupHelperArtifacts(paths, relays, false)
		writeFailure(paths, identity, digest, "control_failed")
		return 1
	}
	if closer, ok := ready.(io.Closer); ok {
		_ = closer.Close()
	}

	leaseDuration := relayLeaseDuration(specs)
	expiresAt := startedAt.Add(leaseDuration)
	var verifyOwnedWorkspace func() bool
	if containerExecutable.Path != "" {
		verifyOwnedWorkspace = func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			return validateOwnedLeaseWorkspace(ctx, containerExecutable, identity) == nil
		}
	}
	nextOwnershipCheck := startedAt.Add(leaseOwnershipCheckInterval(leaseDuration))
	for {
		now := time.Now().UTC()
		if verifyOwnedWorkspace != nil && !now.Before(nextOwnershipCheck) {
			if verifyOwnedWorkspace() {
				now = time.Now().UTC()
				expiresAt = now.Add(leaseDuration)
			}
			nextOwnershipCheck = time.Now().UTC().Add(leaseOwnershipCheckInterval(leaseDuration))
		}
		if !time.Now().UTC().Before(expiresAt) {
			listener.Close()
			cleanupHelperArtifacts(paths, relays, false)
			writeFailure(paths, identity, digest, "expired")
			return 0
		}
		for _, relay := range relays {
			select {
			case <-relay.Done():
				listener.Close()
				code := "relay_failed"
				if !time.Now().UTC().Before(expiresAt) {
					code = "expired"
				}
				cleanupHelperArtifacts(paths, relays, false)
				writeFailure(paths, identity, digest, code)
				return 1
			default:
			}
		}
		deadline := time.Now().Add(250 * time.Millisecond)
		if expiresAt.Before(deadline) {
			deadline = expiresAt
		}
		if err := listener.SetDeadline(deadline); err != nil {
			listener.Close()
			cleanupHelperArtifacts(paths, relays, false)
			writeFailure(paths, identity, digest, "control_failed")
			return 1
		}
		connection, err := listener.AcceptUnix()
		if err != nil {
			if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
				continue
			}
			cleanupHelperArtifacts(paths, relays, false)
			writeFailure(paths, identity, digest, "control_failed")
			return 1
		}
		stop, renewedUntil, response := handleHelperControl(connection, identity, digest, token, executable, result, expiresAt, time.Now().UTC(), leaseDuration, verifyOwnedWorkspace)
		expiresAt = renewedUntil
		if !stop {
			connection.Close()
			continue
		}
		listener.Close()
		cleanupHelperArtifacts(paths, relays, false)
		response.State = "stopped"
		_ = connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = json.NewEncoder(connection).Encode(response)
		connection.Close()
		return 0
	}
}

func handleHelperControl(connection *net.UnixConn, identity LeaseIdentity, digest, token string, executable executableIdentity, result LeaseResult, expiresAt, now time.Time, leaseDuration time.Duration, verifyOwnedWorkspace func() bool) (bool, time.Time, controlResponse) {
	response := controlResponse{Version: 1, State: "error", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	var request controlRequest
	if err := decodeBoundedJSON(connection, MaxControlBytes, &request); err != nil {
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, expiresAt, response
	}
	if request.Version != 1 || request.Identity != identity || request.SpecDigest != digest || !constantTimeTokenEqual(request.Token, token) {
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, expiresAt, response
	}
	switch request.Operation {
	case "status":
		response.State = "running"
		response.Result = cloneLeaseResult(result)
		response.ExpiresAt = expiresAt
		_ = json.NewEncoder(connection).Encode(response)
		return false, expiresAt, response
	case "renew":
		if verifyOwnedWorkspace != nil && !verifyOwnedWorkspace() {
			response.Failure = "control_failed"
			_ = json.NewEncoder(connection).Encode(response)
			return false, expiresAt, response
		}
		candidate := now.Add(leaseDuration)
		if candidate.After(expiresAt) {
			expiresAt = candidate
		}
		response.State = "running"
		response.Result = cloneLeaseResult(result)
		response.ExpiresAt = expiresAt
		_ = json.NewEncoder(connection).Encode(response)
		return false, expiresAt, response
	case "stop":
		return true, expiresAt, response
	default:
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, expiresAt, response
	}
}

func leaseOwnershipCheckInterval(lease time.Duration) time.Duration {
	interval := lease / 2
	if interval > 5*time.Minute {
		return 5 * time.Minute
	}
	if interval < 25*time.Millisecond {
		return 25 * time.Millisecond
	}
	return interval
}

func verifiedHelperPaths(stateRoot string, identity LeaseIdentity) (leasePaths, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return leasePaths{}, errors.New("invalid bridge state root")
	}
	root := filepath.Join(stateRoot, bridgeDirectoryName)
	project := filepath.Join(root, string(identity.ProjectID))
	sandbox := filepath.Join(project, string(identity.Sandbox))
	run := filepath.Join(sandbox, string(identity.RunID))
	for _, path := range []string{stateRoot, root, project, sandbox, run} {
		if err := verifyPrivateDirectory(path); err != nil {
			return leasePaths{}, err
		}
	}
	paths := makeLeasePaths(root, project, sandbox, run)
	return paths, nil
}

func verifyPrivateSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return errors.New("bridge control socket is not private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("bridge control socket owner is invalid")
	}
	return nil
}

func cleanupHelperArtifacts(paths leasePaths, relays []*TCPRelay, preserveFailure bool) {
	for index := len(relays) - 1; index >= 0; index-- {
		_ = relays[index].Close()
	}
	_ = removePrivateSocket(paths.socket)
	_ = removePrivateRegular(paths.ledger)
	_ = removePrivateRegular(paths.token)
	if !preserveFailure {
		_ = removePrivateRegular(paths.failure)
	}
}

func removePrivateRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("refusing to remove unsafe private bridge file")
	}
	return os.Remove(path)
}

func removePrivateSocket(path string) error {
	if err := verifyPrivateSocket(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Remove(path)
}

func writeFailure(paths leasePaths, identity LeaseIdentity, digest, code string) {
	if !validFailureCode(code) {
		code = "control_failed"
	}
	status := failureStatus{Version: 1, Identity: identity, SpecDigest: digest, Failure: code, At: time.Now().UTC()}
	encoded, err := json.Marshal(status)
	if err == nil && len(encoded) <= MaxControlBytes {
		_ = atomicWritePrivate(paths.failure, encoded)
	}
}

func writeHelperReady(writer io.Writer, ready helperReady) bool {
	encoded, err := json.Marshal(ready)
	if err != nil || len(encoded) > MaxControlBytes {
		return false
	}
	encoded = append(encoded, '\n')
	written, err := writer.Write(encoded)
	return err == nil && written == len(encoded)
}
