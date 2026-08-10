package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

const (
	bridgeDirectoryName = "bridges"
	ledgerFileName      = "lease.json"
	tokenFileName       = "control.token"
	failureFileName     = "failure.json"
	controlSocketName   = "control.sock"
	leaseLockName       = ".lease-lock"
	defaultReadyWait    = 15 * time.Second
	defaultStopWait     = 10 * time.Second
)

type executableIdentity struct {
	Path            string `json:"path"`
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	UID             uint32 `json:"uid"`
}

type leaseLedger struct {
	Version          int                `json:"version"`
	Identity         LeaseIdentity      `json:"identity"`
	SpecDigest       string             `json:"spec_digest"`
	PID              int                `json:"pid"`
	ProcessStartedAt time.Time          `json:"process_started_at"`
	Executable       executableIdentity `json:"executable"`
	Result           LeaseResult        `json:"result"`
}

type failureStatus struct {
	Version    int           `json:"version"`
	Identity   LeaseIdentity `json:"identity"`
	SpecDigest string        `json:"spec_digest"`
	Failure    string        `json:"failure"`
	At         time.Time     `json:"at"`
}

type helperEnvelope struct {
	Version             int                `json:"version"`
	StateRoot           string             `json:"state_root"`
	Identity            LeaseIdentity      `json:"identity"`
	Specs               []RelaySpec        `json:"specs"`
	SpecDigest          string             `json:"spec_digest"`
	Token               string             `json:"token"`
	Executable          executableIdentity `json:"executable"`
	ContainerExecutable executableIdentity `json:"container_executable,omitempty"`
}

type controlRequest struct {
	Version    int           `json:"version"`
	Operation  string        `json:"operation"`
	Token      string        `json:"token"`
	Identity   LeaseIdentity `json:"identity"`
	SpecDigest string        `json:"spec_digest"`
}

type controlResponse struct {
	Version    int                `json:"version"`
	State      string             `json:"state"`
	Identity   LeaseIdentity      `json:"identity"`
	SpecDigest string             `json:"spec_digest"`
	PID        int                `json:"pid"`
	Executable executableIdentity `json:"executable"`
	Result     LeaseResult        `json:"result,omitempty"`
	Failure    string             `json:"failure,omitempty"`
	ExpiresAt  time.Time          `json:"expires_at,omitempty"`
}

type helperReady struct {
	Version    int                `json:"version"`
	State      string             `json:"state"`
	Identity   LeaseIdentity      `json:"identity"`
	SpecDigest string             `json:"spec_digest"`
	PID        int                `json:"pid"`
	Executable executableIdentity `json:"executable"`
	Result     LeaseResult        `json:"result,omitempty"`
	Failure    string             `json:"failure,omitempty"`
}

type leasePaths struct {
	root, project, sandbox, run          string
	ledger, token, failure, socket, lock string
}

// ProductionLeaseManager launches the installed dsx executable in its hidden
// helper mode. It never signals a PID; authenticated control is the only stop
// authority.
type ProductionLeaseManager struct {
	stateRoot           string
	executable          executableIdentity
	containerExecutable executableIdentity
	readyWait           time.Duration
	stopWait            time.Duration
	now                 func() time.Time
}

var _ LeaseManager = (*ProductionLeaseManager)(nil)

func NewProductionLeaseManager(stateRoot, executable string) (*ProductionLeaseManager, error) {
	return newProductionLeaseManager(stateRoot, executable, "")
}

// NewProductionLeaseManagerWithContainer pins both the DSX helper executable
// and the Apple CLI used by publication relays.
func NewProductionLeaseManagerWithContainer(stateRoot, executable, containerExecutable string) (*ProductionLeaseManager, error) {
	return newProductionLeaseManager(stateRoot, executable, containerExecutable)
}

func newProductionLeaseManager(stateRoot, executable, containerExecutable string) (*ProductionLeaseManager, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, model.NewError(model.CodeInvalidInput, "bridge state root must be a clean absolute path", nil)
	}
	if err := verifyPrivateDirectory(stateRoot); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "verify bridge state root", err)
	}
	identity, err := canonicalExecutableIdentity(executable)
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "verify dsx bridge helper executable", err)
	}
	var containerIdentity executableIdentity
	if containerExecutable != "" {
		containerIdentity, err = canonicalExecutableIdentity(containerExecutable)
		if err != nil {
			return nil, model.Wrap(model.CodeUnavailable, "verify Apple container executable", err)
		}
	}
	return &ProductionLeaseManager{
		stateRoot: stateRoot, executable: identity, containerExecutable: containerIdentity,
		readyWait: defaultReadyWait, stopWait: defaultStopWait, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (manager *ProductionLeaseManager) Ensure(ctx context.Context, identity LeaseIdentity, specs []RelaySpec) (LeaseResult, error) {
	if ctx == nil {
		return LeaseResult{}, model.NewError(model.CodeInvalidInput, "bridge ensure context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return LeaseResult{}, model.NewError(model.CodeInvalidInput, "invalid bridge lease identity", err)
	}
	canonical, digest, err := validateRelaySpecs(specs)
	if err != nil {
		return LeaseResult{}, model.NewError(model.CodeInvalidInput, "invalid bridge relay lease", err)
	}
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		return LeaseResult{}, err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return LeaseResult{}, err
	}
	defer unlock()
	if err := manager.verifyExecutable(); err != nil {
		return LeaseResult{}, err
	}
	if relaySpecsRequireContainer(canonical) {
		if manager.containerExecutable.Path == "" {
			return LeaseResult{}, model.NewError(model.CodeUnavailable, "Apple container executable is unavailable for publication relay", nil)
		}
		current, executableErr := canonicalExecutableIdentity(manager.containerExecutable.Path)
		if executableErr != nil || current != manager.containerExecutable {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "Apple container executable identity changed", executableErr)
		}
	}

	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge lease ledger is unsafe or corrupt; preserving it", err)
	}
	if found {
		if err := validateLedger(ledger, identity, digest, manager.executable); err != nil || !validLeaseResult(canonical, ledger.Result) {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge lease evidence differs from the requested workspace; preserving it", err)
		}
		token, tokenErr := readPrivateToken(paths.token)
		if tokenErr != nil {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge lease token evidence is unavailable; preserving the lease", tokenErr)
		}
		response, controlErr := manager.control(ctx, paths.socket, controlRequest{Version: 1, Operation: "renew", Token: token, Identity: identity, SpecDigest: digest})
		if controlErr != nil {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge helper cannot prove and renew its live lease; preserving the lease", controlErr)
		}
		if err := validateControlResponse(response, ledger); err != nil {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge helper returned contradictory ownership evidence; preserving the lease", err)
		}
		renewedAfter := manager.now()
		maximumExpiry := renewedAfter.Add(relayLeaseDuration(canonical))
		if !response.ExpiresAt.After(renewedAfter) || response.ExpiresAt.After(maximumExpiry) {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge helper returned an invalid renewal boundary; preserving the lease", nil)
		}
		return cloneLeaseResult(response.Result), nil
	}
	if err := rejectUnexpectedLeaseEvidence(paths, false); err != nil {
		return LeaseResult{}, err
	}
	failed, failedFound, err := loadPrivateJSON[failureStatus](paths.failure, MaxControlBytes)
	if err != nil {
		return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge failure evidence is unsafe or corrupt; preserving it", err)
	}
	if failedFound {
		if failed.Version != 1 || failed.Identity != identity || failed.SpecDigest != digest || !validFailureCode(failed.Failure) {
			return LeaseResult{}, model.NewError(model.CodeAmbiguous, "bridge failure evidence differs from the requested workspace; preserving it", nil)
		}
		if err := os.Remove(paths.failure); err != nil {
			return LeaseResult{}, model.Wrap(model.CodeInternal, "clear exact bridge failure evidence", err)
		}
	}
	return manager.launch(ctx, paths, identity, canonical, digest)
}

func (manager *ProductionLeaseManager) Stop(ctx context.Context, identity LeaseIdentity) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "bridge stop context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid bridge lease identity", err)
	}
	if err := manager.verifyExecutable(); err != nil {
		return err
	}
	paths, exists, err := manager.existingPaths(identity)
	if err != nil || !exists {
		return err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return err
	}
	defer unlock()
	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return model.NewError(model.CodeAmbiguous, "bridge lease ledger is unsafe or corrupt; preserving it", err)
	}
	if !found {
		failure, failed, failureErr := loadPrivateJSON[failureStatus](paths.failure, MaxControlBytes)
		if failureErr != nil {
			return model.NewError(model.CodeAmbiguous, "bridge failure evidence is unsafe or corrupt; preserving it", failureErr)
		}
		if failed {
			if failure.Version != 1 || failure.Identity != identity || !validFailureCode(failure.Failure) {
				return model.NewError(model.CodeAmbiguous, "bridge failure evidence has contradictory ownership; preserving it", nil)
			}
			if err := os.Remove(paths.failure); err != nil {
				return model.Wrap(model.CodeInternal, "remove exact bridge failure evidence", err)
			}
		}
		if err := rejectUnexpectedLeaseEvidence(paths, true); err != nil {
			return err
		}
		return removeEmptyRunDirectory(paths.run)
	}
	if ledger.Version != 1 || ledger.Identity != identity || ledger.PID <= 0 || ledger.Executable != manager.executable {
		return model.NewError(model.CodeAmbiguous, "bridge lease ownership evidence is contradictory; preserving it", nil)
	}
	token, err := readPrivateToken(paths.token)
	if err != nil {
		return model.NewError(model.CodeAmbiguous, "bridge lease token evidence is unavailable; preserving the lease", err)
	}
	response, err := manager.control(ctx, paths.socket, controlRequest{Version: 1, Operation: "stop", Token: token, Identity: identity, SpecDigest: ledger.SpecDigest})
	if err != nil {
		return model.NewError(model.CodeAmbiguous, "bridge helper did not authenticate stop; preserving the lease", err)
	}
	if err := validateControlResponse(response, ledger); err != nil || response.State != "stopped" {
		return model.NewError(model.CodeAmbiguous, "bridge helper returned contradictory stop evidence; preserving the lease", err)
	}
	deadline := time.Now().Add(manager.stopWait)
	for {
		absent, absenceErr := exactLeaseArtifactsAbsent(paths)
		if absenceErr != nil {
			return model.NewError(model.CodeAmbiguous, "verify stopped bridge cleanup", absenceErr)
		}
		if absent {
			return removeEmptyRunDirectory(paths.run)
		}
		if err := ctx.Err(); err != nil {
			return model.Wrap(model.CodeUnavailable, "wait for bridge helper cleanup", err)
		}
		if time.Now().After(deadline) {
			return model.NewError(model.CodeUnavailable, "bridge helper did not finish exact cleanup before the deadline", nil)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (manager *ProductionLeaseManager) Status(ctx context.Context, identity LeaseIdentity) (LeaseStatus, error) {
	if ctx == nil {
		return LeaseStatus{}, model.NewError(model.CodeInvalidInput, "bridge status context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return LeaseStatus{}, model.NewError(model.CodeInvalidInput, "invalid bridge lease identity", err)
	}
	if err := manager.verifyExecutable(); err != nil {
		return LeaseStatus{}, err
	}
	paths, exists, err := manager.existingPaths(identity)
	if err != nil || !exists {
		return LeaseStatus{State: "absent"}, err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return LeaseStatus{}, err
	}
	defer unlock()
	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return LeaseStatus{}, model.NewError(model.CodeAmbiguous, "bridge lease ledger is unsafe or corrupt", err)
	}
	if !found {
		failure, failed, failureErr := loadPrivateJSON[failureStatus](paths.failure, MaxControlBytes)
		if failureErr != nil {
			return LeaseStatus{}, model.NewError(model.CodeAmbiguous, "bridge failure evidence is unsafe or corrupt", failureErr)
		}
		if failed {
			if failure.Version != 1 || failure.Identity != identity || !validFailureCode(failure.Failure) {
				return LeaseStatus{}, model.NewError(model.CodeAmbiguous, "bridge failure evidence has contradictory ownership", nil)
			}
			return LeaseStatus{State: "error", Failure: failure.Failure}, nil
		}
		if err := rejectUnexpectedLeaseEvidence(paths, true); err != nil {
			return LeaseStatus{}, err
		}
		return LeaseStatus{State: "absent"}, nil
	}
	if ledger.Version != 1 || ledger.Identity != identity || ledger.Executable != manager.executable {
		return LeaseStatus{}, model.NewError(model.CodeAmbiguous, "bridge lease ownership evidence is contradictory", nil)
	}
	token, err := readPrivateToken(paths.token)
	if err != nil {
		return LeaseStatus{State: "dead"}, nil
	}
	response, err := manager.control(ctx, paths.socket, controlRequest{Version: 1, Operation: "status", Token: token, Identity: identity, SpecDigest: ledger.SpecDigest})
	if err != nil {
		return LeaseStatus{State: "dead"}, nil
	}
	if err := validateControlResponse(response, ledger); err != nil {
		return LeaseStatus{}, model.NewError(model.CodeAmbiguous, "bridge helper returned contradictory ownership evidence", err)
	}
	return LeaseStatus{State: "running", Result: cloneLeaseResult(response.Result)}, nil
}

func (manager *ProductionLeaseManager) launch(ctx context.Context, paths leasePaths, identity LeaseIdentity, specs []RelaySpec, digest string) (LeaseResult, error) {
	if err := rejectUnexpectedLeaseEvidence(paths, true); err != nil {
		return LeaseResult{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return LeaseResult{}, model.Wrap(model.CodeInternal, "generate bridge control token", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	if err := atomicWritePrivate(paths.token, []byte(token+"\n")); err != nil {
		return LeaseResult{}, model.Wrap(model.CodeInternal, "write bridge control token", err)
	}
	cleanupToken := true
	defer func() {
		if cleanupToken {
			_ = os.Remove(paths.token)
		}
	}()
	envelope := helperEnvelope{
		Version: 1, StateRoot: manager.stateRoot, Identity: identity, Specs: specs, SpecDigest: digest,
		Token: token, Executable: manager.executable, ContainerExecutable: manager.containerExecutable,
	}
	input, err := json.Marshal(envelope)
	if err != nil || len(input) > MaxHelperInputBytes {
		return LeaseResult{}, model.NewError(model.CodeInternal, "encode bounded bridge helper input", err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return LeaseResult{}, model.Wrap(model.CodeInternal, "create bridge helper readiness pipe", err)
	}
	defer readyReader.Close()
	command := exec.Command(manager.executable.Path, "__bridge-helper")
	command.Dir = paths.run
	command.Stdin = bytes.NewReader(input)
	command.Stdout = nil
	command.Stderr = nil
	command.ExtraFiles = []*os.File{readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return LeaseResult{}, model.Wrap(model.CodeUnavailable, "start bridge helper", err)
	}
	cleanupToken = false
	readyWriter.Close()
	defer command.Process.Release()
	readyContext, cancel := context.WithTimeout(ctx, manager.readyWait)
	defer cancel()
	result := make(chan struct {
		ready helperReady
		err   error
	}, 1)
	go func() {
		var ready helperReady
		err := decodeBoundedJSON(readyReader, MaxControlBytes, &ready)
		result <- struct {
			ready helperReady
			err   error
		}{ready: ready, err: err}
	}()
	var ready helperReady
	select {
	case <-readyContext.Done():
		return LeaseResult{}, model.Wrap(model.CodeUnavailable, "wait for bridge helper readiness", readyContext.Err())
	case decoded := <-result:
		if decoded.err != nil {
			return LeaseResult{}, model.Wrap(model.CodeUnavailable, "read bridge helper readiness", decoded.err)
		}
		ready = decoded.ready
	}
	acceptedAuthority := ready.Version == 1 && ready.State == "ready" && ready.Identity == identity && ready.SpecDigest == digest && ready.PID == command.Process.Pid && ready.Executable == manager.executable
	stopAccepted := func(cause error) error {
		if !acceptedAuthority {
			return cause
		}
		response, stopErr := manager.control(context.WithoutCancel(ctx), paths.socket, controlRequest{Version: 1, Operation: "stop", Token: token, Identity: identity, SpecDigest: digest})
		if stopErr != nil || response.Version != 1 || response.State != "stopped" || response.Identity != identity || response.SpecDigest != digest || response.PID != ready.PID || response.Executable != manager.executable {
			return errors.Join(cause, model.NewError(model.CodeAmbiguous, "accepted bridge helper could not prove authenticated startup rollback; preserving its authority", stopErr))
		}
		deadline := time.Now().Add(manager.stopWait)
		for {
			absent, absenceErr := exactLeaseArtifactsAbsent(paths)
			if absenceErr != nil {
				return errors.Join(cause, model.NewError(model.CodeAmbiguous, "verify bridge startup rollback cleanup", absenceErr))
			}
			if absent {
				_ = removeEmptyRunDirectory(paths.run)
				return cause
			}
			if time.Now().After(deadline) {
				return errors.Join(cause, model.NewError(model.CodeUnavailable, "accepted bridge helper did not finish startup rollback cleanup", nil))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !acceptedAuthority || !validLeaseResult(specs, ready.Result) {
		return LeaseResult{}, stopAccepted(model.NewError(model.CodeAmbiguous, "bridge helper readiness evidence differs from the requested lease", nil))
	}
	ledger, found, err := loadPrivateJSON[leaseLedger](paths.ledger, MaxControlBytes)
	if err != nil || !found {
		return LeaseResult{}, stopAccepted(model.NewError(model.CodeAmbiguous, "bridge helper did not publish a valid private ledger", err))
	}
	if err := validateLedger(ledger, identity, digest, manager.executable); err != nil || ledger.PID != ready.PID || !equalLeaseResult(ledger.Result, ready.Result) {
		return LeaseResult{}, stopAccepted(model.NewError(model.CodeAmbiguous, "bridge helper ledger differs from readiness evidence", err))
	}
	return cloneLeaseResult(ready.Result), nil
}

func (manager *ProductionLeaseManager) control(ctx context.Context, socket string, request controlRequest) (controlResponse, error) {
	var response controlResponse
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxControlBytes {
		return response, errors.New("invalid bounded bridge control request")
	}
	controlContext, cancel := context.WithTimeout(ctx, manager.stopWait)
	defer cancel()
	output := &boundedOutput{maximum: MaxControlBytes}
	command := exec.CommandContext(controlContext, manager.executable.Path, "__bridge-control")
	command.Dir = filepath.Dir(socket)
	command.Stdin = bytes.NewReader(encoded)
	command.Stdout = output
	command.Stderr = nil
	if err := command.Run(); err != nil {
		return response, err
	}
	if err := decodeBoundedJSON(bytes.NewReader(output.bytes), MaxControlBytes, &response); err != nil {
		return response, err
	}
	return response, nil
}

type boundedOutput struct {
	bytes   []byte
	maximum int
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > output.maximum-len(output.bytes) {
		return 0, errors.New("bridge control output exceeds size limit")
	}
	output.bytes = append(output.bytes, data...)
	return len(data), nil
}

func (manager *ProductionLeaseManager) ensurePaths(identity LeaseIdentity) (leasePaths, error) {
	if err := verifyPrivateDirectory(manager.stateRoot); err != nil {
		return leasePaths{}, model.Wrap(model.CodeUnavailable, "verify bridge state root", err)
	}
	root, err := ensurePrivateChildDirectory(manager.stateRoot, bridgeDirectoryName)
	if err != nil {
		return leasePaths{}, err
	}
	project, err := ensurePrivateChildDirectory(root, string(identity.ProjectID))
	if err != nil {
		return leasePaths{}, err
	}
	sandbox, err := ensurePrivateChildDirectory(project, string(identity.Sandbox))
	if err != nil {
		return leasePaths{}, err
	}
	run, err := ensurePrivateChildDirectory(sandbox, string(identity.RunID))
	if err != nil {
		return leasePaths{}, err
	}
	paths := makeLeasePaths(root, project, sandbox, run)
	return paths, nil
}

func (manager *ProductionLeaseManager) existingPaths(identity LeaseIdentity) (leasePaths, bool, error) {
	root := filepath.Join(manager.stateRoot, bridgeDirectoryName)
	project := filepath.Join(root, string(identity.ProjectID))
	sandbox := filepath.Join(project, string(identity.Sandbox))
	run := filepath.Join(sandbox, string(identity.RunID))
	for _, path := range []string{manager.stateRoot, root, project, sandbox} {
		if err := verifyPrivateDirectory(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return leasePaths{}, false, nil
			}
			return leasePaths{}, false, model.NewError(model.CodeAmbiguous, "bridge state ancestry is unsafe", err)
		}
	}
	if err := verifyPrivateDirectory(run); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return leasePaths{}, false, nil
		}
		return leasePaths{}, false, model.NewError(model.CodeAmbiguous, "bridge lease directory is unsafe", err)
	}
	return makeLeasePaths(root, project, sandbox, run), true, nil
}

func makeLeasePaths(root, project, sandbox, run string) leasePaths {
	return leasePaths{root: root, project: project, sandbox: sandbox, run: run, ledger: filepath.Join(run, ledgerFileName), token: filepath.Join(run, tokenFileName), failure: filepath.Join(run, failureFileName), socket: filepath.Join(run, controlSocketName), lock: filepath.Join(sandbox, leaseLockName)}
}

func relaySpecsRequireContainer(specs []RelaySpec) bool {
	for _, spec := range specs {
		if spec.Mode == RelayModePublication {
			return true
		}
	}
	return false
}

func (manager *ProductionLeaseManager) verifyExecutable() error {
	current, err := canonicalExecutableIdentity(manager.executable.Path)
	if err != nil || current != manager.executable {
		return model.NewError(model.CodeAmbiguous, "dsx bridge helper executable identity changed", err)
	}
	return nil
}

func canonicalExecutableIdentity(path string) (executableIdentity, error) {
	if path == "" {
		return executableIdentity{}, errors.New("empty executable path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return executableIdentity{}, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return executableIdentity{}, err
	}
	if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
		return executableIdentity{}, errors.New("executable path is not canonical")
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return executableIdentity{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return executableIdentity{}, errors.New("executable is not a protected regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return executableIdentity{}, errors.New("executable filesystem identity is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0 {
		return executableIdentity{}, errors.New("executable owner is not trusted")
	}
	return executableIdentity{Path: canonical, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), UID: stat.Uid}, nil
}

func validateLedger(ledger leaseLedger, identity LeaseIdentity, digest string, executable executableIdentity) error {
	if ledger.Version != 1 || ledger.Identity != identity || ledger.SpecDigest != digest || ledger.PID <= 0 || ledger.ProcessStartedAt.IsZero() || ledger.Executable != executable || len(ledger.Result.Bindings) == 0 {
		return errors.New("invalid bridge lease ledger identity")
	}
	return nil
}

func validateControlResponse(response controlResponse, ledger leaseLedger) error {
	if response.Version != 1 || response.Identity != ledger.Identity || response.SpecDigest != ledger.SpecDigest || response.PID != ledger.PID || response.Executable != ledger.Executable || (response.State != "running" && response.State != "stopped") {
		return errors.New("invalid bridge control response")
	}
	if response.State == "running" && !equalLeaseResult(response.Result, ledger.Result) {
		return errors.New("bridge control result mismatch")
	}
	return nil
}

func validLeaseResult(specs []RelaySpec, result LeaseResult) bool {
	if len(result.Bindings) != len(specs) {
		return false
	}
	privateCount := 0
	for index, spec := range specs {
		binding := result.Bindings[index]
		if binding.Name != spec.Name || binding.Mode != spec.Mode || binding.Addr.Unmap() != spec.ListenerIP.Unmap() || binding.Port == 0 || (spec.ListenerPort != 0 && binding.Port != spec.ListenerPort) {
			return false
		}
		if spec.Mode == RelayModePrivateHost {
			privateCount++
			base, err := relayEnvironmentBase(spec.Name)
			if err != nil || result.Environment[base+"_HOST"] != binding.Addr.String() || result.Environment[base+"_PORT"] != strconv.Itoa(int(binding.Port)) {
				return false
			}
		}
	}
	return len(result.Environment) == privateCount*2
}

func cloneLeaseResult(source LeaseResult) LeaseResult {
	return LeaseResult{Bindings: append([]ListenerBinding(nil), source.Bindings...), Environment: cloneStringMap(source.Environment)}
}

func equalLeaseResult(first, second LeaseResult) bool {
	if len(first.Bindings) != len(second.Bindings) || !equalStringMap(first.Environment, second.Environment) {
		return false
	}
	for index := range first.Bindings {
		if first.Bindings[index] != second.Bindings[index] {
			return false
		}
	}
	return true
}

func readPrivateToken(path string) (string, error) {
	data, err := readPrivateFile(path, 256)
	if err != nil {
		return "", err
	}
	token := string(bytes.TrimSpace(data))
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid bridge control token")
	}
	return token, nil
}

func constantTimeTokenEqual(first, second string) bool {
	return len(first) == len(second) && subtle.ConstantTimeCompare([]byte(first), []byte(second)) == 1
}

func validFailureCode(value string) bool {
	switch value {
	case "expired", "relay_failed", "control_failed", "startup_failed":
		return true
	default:
		return false
	}
}

func exactLeaseArtifactsAbsent(paths leasePaths) (bool, error) {
	for _, path := range []string{paths.ledger, paths.token, paths.socket} {
		_, err := os.Lstat(path)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}

func rejectUnexpectedLeaseEvidence(paths leasePaths, allowFailure bool) error {
	for _, path := range []string{paths.token, paths.socket} {
		if _, err := os.Lstat(path); err == nil {
			return model.NewError(model.CodeAmbiguous, "bridge lease has incomplete ownership evidence; preserving it", nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !allowFailure {
		if _, err := os.Lstat(paths.failure); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func verifyPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("path is not a private real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("private directory owner is invalid")
	}
	return nil
}

func ensurePrivateChildDirectory(parent, name string) (string, error) {
	if err := verifyPrivateDirectory(parent); err != nil {
		return "", model.NewError(model.CodeAmbiguous, "bridge state parent is unsafe", err)
	}
	path := filepath.Join(parent, name)
	if filepath.Dir(path) != parent {
		return "", model.NewError(model.CodeInvalidInput, "invalid bridge state component", nil)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", model.Wrap(model.CodeInternal, "create private bridge state directory", err)
	}
	if err := verifyPrivateDirectory(path); err != nil {
		return "", model.NewError(model.CodeAmbiguous, "bridge state directory is unsafe", err)
	}
	return path, nil
}

func readPrivateFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("private file type, mode, or size is invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("private file owner is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private file changed while opening")
	}
	return io.ReadAll(io.LimitReader(file, maximum+1))
}

func loadPrivateJSON[T any](path string, maximum int64) (T, bool, error) {
	var value T
	data, err := readPrivateFile(path, maximum)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if err := decodeBoundedJSON(bytes.NewReader(data), maximum, &value); err != nil {
		return value, false, err
	}
	return value, true, nil
}

func decodeBoundedJSON(reader io.Reader, maximum int64, destination any) error {
	limited := &io.LimitedReader{R: reader, N: maximum + 1}
	decoder := json.NewDecoder(bufio.NewReader(limited))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	if limited.N <= 0 {
		return errors.New("JSON exceeds size limit")
	}
	return nil
}

func atomicWritePrivate(path string, data []byte) error {
	if len(data) > MaxControlBytes {
		return errors.New("private bridge file exceeds size limit")
	}
	directory := filepath.Dir(path)
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Uid != uint32(os.Geteuid()) {
			return errors.New("refusing to replace unsafe private bridge file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := verifyPrivateDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".bridge-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func acquireLeaseLock(ctx context.Context, path string) (func() error, error) {
	if err := verifyPrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "open bridge lease lock", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, model.NewError(model.CodeAmbiguous, "bridge lease lock is unsafe", statErr)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, model.NewError(model.CodeAmbiguous, "bridge lease lock owner is unsafe", nil)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() error { return errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close()) }, nil
		} else if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			file.Close()
			return nil, model.Wrap(model.CodeInternal, "lock bridge lease", err)
		}
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, model.Wrap(model.CodeConflict, "wait for bridge lease lock", err)
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, model.NewError(model.CodeConflict, "timed out waiting for bridge lease lock", nil)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func removeEmptyRunDirectory(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func equalStringMap(first, second map[string]string) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if second[key] != value {
			return false
		}
	}
	return true
}
