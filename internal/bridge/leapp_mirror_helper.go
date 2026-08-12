package bridge

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

func runLeappMirrorStart(command leappMirrorCommand) int {
	ready := os.NewFile(uintptr(3), "leapp-mirror-ready")
	acceptance := os.NewFile(uintptr(4), "leapp-mirror-acceptance")
	if ready == nil || acceptance == nil {
		return 2
	}
	defer ready.Close()
	defer acceptance.Close()
	readyInfo, readyErr := ready.Stat()
	acceptanceInfo, acceptanceErr := acceptance.Stat()
	if readyErr != nil || acceptanceErr != nil || readyInfo.Mode()&os.ModeNamedPipe == 0 || readyInfo.Mode()&os.ModeCharDevice != 0 || acceptanceInfo.Mode()&os.ModeNamedPipe == 0 || acceptanceInfo.Mode()&os.ModeCharDevice != 0 {
		return 2
	}
	_ = os.Stdin.Close()
	_ = os.Stdout.Close()
	_ = os.Stderr.Close()
	startupFailure := func(code string) int {
		writeLeappMirrorResponse(ready, leappMirrorResponse{Version: 1, State: "error", Identity: command.Identity, SpecDigest: command.SpecDigest, Failure: code})
		return 2
	}
	if command.StateRoot == "" || command.SpecDigest == "" || command.Token == "" || command.Identity.Validate() != nil {
		return startupFailure("startup_failed")
	}
	authority := leappAuthorityForSpec(command.Spec)
	validatedSpec, digest, err := validatedLeappMirrorSpec(authority)
	if err != nil || validatedSpec != command.Spec || digest != command.SpecDigest {
		return startupFailure("startup_failed")
	}
	executablePath, err := os.Executable()
	if err != nil {
		return startupFailure("startup_failed")
	}
	executable, err := canonicalExecutableIdentity(executablePath)
	if err != nil || executable != command.Executable {
		return startupFailure("startup_failed")
	}
	paths, err := verifiedLeappMirrorHelperPaths(command.StateRoot, command.Identity)
	if err != nil {
		return startupFailure("startup_failed")
	}
	token, err := readPrivateToken(paths.token)
	if err != nil || !constantTimeTokenEqual(token, command.Token) {
		writeLeappMirrorFailure(paths, command.Identity, digest, "startup_failed")
		return startupFailure("startup_failed")
	}
	return runVerifiedLeappMirrorHelper(ready, acceptance, paths, command.Identity, command.Spec, digest, token, executable)
}

func runVerifiedLeappMirrorHelper(ready io.Writer, acceptance io.Reader, paths leappMirrorPaths, identity LeaseIdentity, spec leappMirrorSpec, digest, token string, executable executableIdentity) int {
	startedAt := time.Now().UTC()
	fail := func(listener *net.UnixListener, code string) int {
		if listener != nil {
			_ = listener.Close()
		}
		cleanupLeappMirrorHelper(paths)
		writeLeappMirrorFailure(paths, identity, digest, code)
		writeLeappMirrorResponse(ready, leappMirrorResponse{Version: 1, State: "error", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable, Failure: code})
		return 1
	}
	mirror, err := ensurePrivateChildDirectory(paths.run, leappMirrorDataName)
	if err != nil || mirror != paths.mirror {
		return fail(nil, "startup_failed")
	}
	configDigest, credentialsDigest, code, err := synchronizeLeappMirror(paths.mirror, spec, [32]byte{}, [32]byte{})
	if err != nil {
		return fail(nil, code)
	}
	if _, err := os.Lstat(paths.socket); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fail(nil, "startup_failed")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: leappMirrorSocketName, Net: "unix"})
	if err != nil {
		return fail(nil, "startup_failed")
	}
	if err := os.Chmod(paths.socket, 0o600); err != nil || verifyPrivateSocket(paths.socket) != nil {
		return fail(listener, "startup_failed")
	}
	ledger := leappMirrorLedger{Version: 1, Identity: identity, Spec: spec, SpecDigest: digest, PID: os.Getpid(), ProcessStartedAt: startedAt, Executable: executable}
	encoded, err := json.Marshal(ledger)
	if err != nil || len(encoded) > MaxControlBytes || atomicWritePrivate(paths.ledger, encoded) != nil {
		return fail(listener, "startup_failed")
	}
	_ = removePrivateRegular(paths.failure)
	if !writeLeappMirrorResponse(ready, leappMirrorResponse{Version: 1, State: "ready", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable}) {
		return fail(listener, "control_failed")
	}
	if closer, ok := ready.(io.Closer); ok {
		_ = closer.Close()
	}
	if !awaitLeappMirrorAcceptance(acceptance, defaultReadyWait) {
		return fail(listener, "control_failed")
	}
	for {
		if err := listener.SetDeadline(time.Now().Add(leappMirrorPollInterval)); err != nil {
			return fail(listener, "control_failed")
		}
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			stop, response := handleLeappMirrorControl(connection, identity, digest, token, executable)
			if !stop {
				_ = connection.Close()
				continue
			}
			_ = listener.Close()
			cleanupLeappMirrorHelper(paths)
			response.State = "stopped"
			_ = connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = json.NewEncoder(connection).Encode(response)
			_ = connection.Close()
			return 0
		}
		if networkErr, ok := acceptErr.(net.Error); !ok || !networkErr.Timeout() {
			return fail(listener, "control_failed")
		}
		nextConfig, nextCredentials, _, syncErr := synchronizeLeappMirror(paths.mirror, spec, configDigest, credentialsDigest)
		if syncErr == nil {
			configDigest, credentialsDigest = nextConfig, nextCredentials
		}
	}
}

func awaitLeappMirrorAcceptance(reader io.Reader, timeout time.Duration) bool {
	result := make(chan bool, 1)
	go func() {
		var accepted [1]byte
		_, err := io.ReadFull(reader, accepted[:])
		result <- err == nil && accepted[0] == 1
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case accepted := <-result:
		return accepted
	case <-timer.C:
		return false
	}
}

func writeLeappMirrorAcceptance(writer io.Writer) bool {
	written, err := writer.Write([]byte{1})
	return err == nil && written == 1
}

func synchronizeLeappMirror(mirror string, spec leappMirrorSpec, oldConfig, oldCredentials [32]byte) ([32]byte, [32]byte, string, error) {
	return synchronizeLeappMirrorWithSnapshot(mirror, spec, oldConfig, oldCredentials, func() (LeappDirectorySnapshot, string, error) {
		directory, err := OpenApprovedLeappDirectory(leappAuthorityForSpec(spec))
		if err != nil {
			switch {
			case errors.Is(err, ErrLeappSourceOversized):
				return LeappDirectorySnapshot{}, "source_oversized", ErrLeappSourceOversized
			case errors.Is(err, ErrLeappSourceUnsafe):
				return LeappDirectorySnapshot{}, "source_unsafe", ErrLeappSourceUnsafe
			default:
				return LeappDirectorySnapshot{}, "source_identity_changed", ErrLeappSourceIdentity
			}
		}
		snapshot, snapshotErr := directory.Snapshot()
		closeErr := directory.Close()
		if snapshotErr != nil {
			code := "source_unsafe"
			if errors.Is(snapshotErr, ErrLeappSourceOversized) {
				code = "source_oversized"
			}
			return LeappDirectorySnapshot{}, code, snapshotErr
		}
		if closeErr != nil {
			return LeappDirectorySnapshot{}, "source_unsafe", ErrLeappSourceUnsafe
		}
		return snapshot, "", nil
	})
}

func synchronizeLeappMirrorWithSnapshot(mirror string, _ leappMirrorSpec, oldConfig, oldCredentials [32]byte, readSnapshot func() (LeappDirectorySnapshot, string, error)) ([32]byte, [32]byte, string, error) {
	for attempt := range 3 {
		snapshot, code, err := readSnapshot()
		if err != nil {
			return oldConfig, oldCredentials, code, err
		}
		configDigest := sha256.Sum256(snapshot.Config)
		credentialsDigest := sha256.Sum256(snapshot.Credentials)
		if oldConfig != ([32]byte{}) && configDigest != oldConfig && credentialsDigest == oldCredentials {
			if attempt < 2 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return oldConfig, oldCredentials, "source_unsafe", errors.New("Leapp paired source rotation did not complete")
		}
		if configDigest == oldConfig && credentialsDigest == oldCredentials {
			return oldConfig, oldCredentials, "", nil
		}
		if err := publishLeappMirrorGeneration(mirror, snapshot, nil); err != nil {
			return oldConfig, oldCredentials, "source_unsafe", err
		}
		return configDigest, credentialsDigest, "", nil
	}
	return oldConfig, oldCredentials, "source_unsafe", errors.New("Leapp paired source rotation did not stabilize")
}

func publishLeappMirrorGeneration(mirror string, snapshot LeappDirectorySnapshot, beforeCommit func() error) error {
	if err := verifyPrivateDirectory(mirror); err != nil {
		return err
	}
	previous, err := currentLeappMirrorGeneration(mirror)
	if err != nil {
		return err
	}
	generation, err := os.MkdirTemp(mirror, leappMirrorGenerationPrefix)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = removeStagedLeappMirrorGeneration(generation)
		}
	}()
	if err := os.Chmod(generation, 0o700); err != nil {
		return err
	}
	if err := atomicWriteLeappMirrorFile(generation, leappConfigFile, snapshot.Config); err != nil {
		return err
	}
	if err := atomicWriteLeappMirrorFile(generation, leappCredentialsFile, snapshot.Credentials); err != nil {
		return err
	}
	if err := verifyLeappMirrorGeneration(generation); err != nil {
		return err
	}
	directory, err := os.Open(generation)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return err
		}
	}
	temporary, err := os.CreateTemp(mirror, leappMirrorCurrentPrefix+"*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return err
	}
	if err := os.Remove(temporaryName); err != nil {
		return err
	}
	defer os.Remove(temporaryName)
	if err := os.Symlink(filepath.Base(generation), temporaryName); err != nil {
		return err
	}
	current := filepath.Join(mirror, leappMirrorCurrentName)
	if err := os.Rename(temporaryName, current); err != nil {
		return err
	}
	mirrorDirectory, err := os.Open(mirror)
	if err != nil {
		_ = restoreLeappMirrorCurrent(mirror, previous)
		return err
	}
	syncErr = mirrorDirectory.Sync()
	closeErr = mirrorDirectory.Close()
	if syncErr != nil || closeErr != nil {
		_ = restoreLeappMirrorCurrent(mirror, previous)
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	published = true
	if previous != "" && previous != generation {
		_ = removeLeappMirrorGeneration(previous)
	}
	return nil
}

func restoreLeappMirrorCurrent(mirror, generation string) error {
	current := filepath.Join(mirror, leappMirrorCurrentName)
	if generation == "" {
		return os.Remove(current)
	}
	temporary, err := os.CreateTemp(mirror, leappMirrorCurrentPrefix+"rollback-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return err
	}
	if err := os.Remove(temporaryName); err != nil {
		return err
	}
	defer os.Remove(temporaryName)
	if err := os.Symlink(filepath.Base(generation), temporaryName); err != nil {
		return err
	}
	return os.Rename(temporaryName, current)
}

func handleLeappMirrorControl(connection *net.UnixConn, identity LeaseIdentity, digest, token string, executable executableIdentity) (bool, leappMirrorResponse) {
	response := leappMirrorResponse{Version: 1, State: "error", Identity: identity, SpecDigest: digest, PID: os.Getpid(), Executable: executable}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	var request leappMirrorCommand
	if err := decodeBoundedJSON(connection, MaxControlBytes, &request); err != nil {
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, response
	}
	if !validLeappMirrorControlRequest(request, identity, digest, token) {
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, response
	}
	switch request.Action {
	case "status":
		response.State = "running"
		_ = json.NewEncoder(connection).Encode(response)
		return false, response
	case "stop":
		return true, response
	default:
		response.Failure = "control_failed"
		_ = json.NewEncoder(connection).Encode(response)
		return false, response
	}
}

func validLeappMirrorControlRequest(request leappMirrorCommand, identity LeaseIdentity, digest, token string) bool {
	return request.Version == 1 && request.Identity == identity && request.SpecDigest == digest && constantTimeTokenEqual(request.Token, token) && (request.Action == "status" || request.Action == "stop")
}
func verifiedLeappMirrorHelperPaths(stateRoot string, identity LeaseIdentity) (leappMirrorPaths, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return leappMirrorPaths{}, errors.New("invalid Leapp mirror state root")
	}
	root := filepath.Join(stateRoot, leappMirrorDirectoryName)
	project := filepath.Join(root, string(identity.ProjectID))
	workspace := filepath.Join(project, string(identity.Workspace))
	run := filepath.Join(workspace, string(identity.RunID))
	for _, path := range []string{stateRoot, root, project, workspace, run} {
		if err := verifyPrivateDirectory(path); err != nil {
			return leappMirrorPaths{}, err
		}
	}
	return makeLeappMirrorPaths(root, project, workspace, run), nil
}

func cleanupLeappMirrorHelper(paths leappMirrorPaths) {
	_ = removePrivateSocket(paths.socket)
	_ = removePrivateRegular(paths.ledger)
	_ = removePrivateRegular(paths.token)
	_ = removeLeappMirrorDirectory(paths.mirror)
}

func writeLeappMirrorFailure(paths leappMirrorPaths, identity LeaseIdentity, digest, code string) {
	if !validLeappMirrorFailure(code) {
		code = "control_failed"
	}
	failure := leappMirrorFailure{Version: 1, Identity: identity, SpecDigest: digest, Failure: code, At: time.Now().UTC()}
	encoded, err := json.Marshal(failure)
	if err == nil && len(encoded) <= MaxControlBytes {
		_ = atomicWritePrivate(paths.failure, encoded)
	}
}

func writeLeappMirrorResponse(writer io.Writer, response leappMirrorResponse) bool {
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > MaxControlBytes {
		return false
	}
	encoded = append(encoded, '\n')
	written, err := writer.Write(encoded)
	return err == nil && written == len(encoded)
}

func atomicWriteLeappMirrorFile(mirror, name string, contents []byte) error {
	if (name != leappConfigFile && name != leappCredentialsFile) || len(contents) > MaxLeappFileBytes {
		return errors.New("invalid bounded Leapp mirror update")
	}
	if err := verifyPrivateDirectory(mirror); err != nil {
		return err
	}
	target := filepath.Join(mirror, name)
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			return errors.New("refusing to replace unsafe Leapp mirror file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging := filepath.Dir(mirror)
	if err := verifyPrivateDirectory(staging); err != nil {
		return err
	}
	file, err := os.CreateTemp(staging, leappMirrorWritePrefix+"*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o400); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	directory, err := os.Open(mirror)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
