package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/model"
)

type leappMirrorLedger struct {
	Version          int                `json:"version"`
	Identity         LeaseIdentity      `json:"identity"`
	Spec             leappMirrorSpec    `json:"spec"`
	SpecDigest       string             `json:"spec_digest"`
	PID              int                `json:"pid"`
	ProcessStartedAt time.Time          `json:"process_started_at"`
	Executable       executableIdentity `json:"executable"`
}

type leappMirrorFailure struct {
	Version    int           `json:"version"`
	Identity   LeaseIdentity `json:"identity"`
	SpecDigest string        `json:"spec_digest"`
	Failure    string        `json:"failure"`
	At         time.Time     `json:"at"`
}

type leappMirrorCommand struct {
	Version    int                `json:"version"`
	Action     string             `json:"action"`
	StateRoot  string             `json:"state_root,omitempty"`
	Identity   LeaseIdentity      `json:"identity"`
	Spec       leappMirrorSpec    `json:"spec,omitempty"`
	SpecDigest string             `json:"spec_digest"`
	Token      string             `json:"token"`
	Executable executableIdentity `json:"executable,omitempty"`
}

type leappMirrorResponse struct {
	Version    int                `json:"version"`
	State      string             `json:"state"`
	Identity   LeaseIdentity      `json:"identity"`
	SpecDigest string             `json:"spec_digest"`
	PID        int                `json:"pid"`
	Executable executableIdentity `json:"executable"`
	Failure    string             `json:"failure,omitempty"`
}

type leappMirrorPaths struct {
	root, project, workspace, run                string
	ledger, token, failure, socket, lock, mirror string
}

// ProductionHostAWSWorkspaceManager launches the installed dsx executable in
// hidden helper mode while preserving a stable workspace publication path.
type ProductionHostAWSWorkspaceManager struct {
	stateRoot       string
	executable      executableIdentity
	readyWait       time.Duration
	stopWait        time.Duration
	now             func() time.Time
	loadReadyLedger func(string) (leappMirrorLedger, bool, error)
	verifySnapshot  func(string) error
	controlOverride func(context.Context, leappMirrorPaths, leappMirrorCommand) (leappMirrorResponse, error)
	launchOverride  func(context.Context, leappMirrorPaths, LeaseIdentity, leappMirrorSpec, string) (string, error)
	artifactsAbsent func(leappMirrorPaths) (bool, error)
	processAlive    func(int) (bool, error)
	verifySocket    func(string) error
}

var _ HostAWSWorkspaceManager = (*ProductionHostAWSWorkspaceManager)(nil)

func NewProductionHostAWSWorkspaceManager(stateRoot, executable string) (*ProductionHostAWSWorkspaceManager, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, model.NewError(model.CodeInvalidInput, "host AWS workspace state root must be a clean absolute path", nil)
	}
	if err := verifyPrivateDirectory(stateRoot); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "verify host AWS workspace state root", err)
	}
	identity, err := canonicalExecutableIdentity(executable)
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "verify dsx host AWS helper executable", err)
	}
	return &ProductionHostAWSWorkspaceManager{
		stateRoot: stateRoot, executable: identity, readyWait: defaultReadyWait, stopWait: defaultStopWait,
		now: func() time.Time { return time.Now().UTC() },
		loadReadyLedger: func(path string) (leappMirrorLedger, bool, error) {
			return loadPrivateJSON[leappMirrorLedger](path, MaxControlBytes)
		},
		verifySnapshot:  verifyLeappMirrorSnapshot,
		artifactsAbsent: exactLeappMirrorHelperArtifactsAbsent,
		processAlive:    leappMirrorProcessAlive,
		verifySocket:    verifyPrivateSocket,
	}, nil
}

func (manager *ProductionHostAWSWorkspaceManager) Prepare(ctx context.Context, identity LeaseIdentity) (string, error) {
	if ctx == nil {
		return "", model.NewError(model.CodeInvalidInput, "host AWS workspace prepare context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return "", model.NewError(model.CodeInvalidInput, "invalid host AWS workspace identity", err)
	}
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		return "", err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := manager.prepareLocked(paths, identity); err != nil {
		return "", err
	}
	return paths.mirror, nil
}

func (manager *ProductionHostAWSWorkspaceManager) Enable(ctx context.Context, identity LeaseIdentity, authority HostAWSAuthority) (string, error) {
	if ctx == nil {
		return "", model.NewError(model.CodeInvalidInput, "host AWS workspace enable context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return "", model.NewError(model.CodeInvalidInput, "invalid host AWS workspace identity", err)
	}
	spec, digest, err := validatedLeappMirrorSpec(authority)
	if err != nil {
		return "", model.NewError(model.CodeInvalidInput, "invalid host AWS authority", err)
	}
	if err := manager.verifyExecutable(); err != nil {
		return "", err
	}
	paths, err := manager.ensurePaths(identity)
	if err != nil {
		return "", err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return "", err
	}
	defer unlock()
	ledger, found, err := loadPrivateJSON[leappMirrorLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return "", model.NewError(model.CodeAmbiguous, "host AWS helper ledger is unsafe or corrupt; preserving it", err)
	}
	if found {
		if err := validateLeappMirrorLedger(ledger, identity, spec, digest, manager.executable); err != nil {
			return "", model.NewError(model.CodeAmbiguous, "host AWS helper evidence differs from the requested workspace; preserving it", err)
		}
		if _, err := os.Lstat(paths.failure); err == nil {
			return "", model.NewError(model.CodeAmbiguous, "active host AWS helper has contradictory failure evidence; preserving it", nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := rejectUnexpectedHostAWSRunEvidence(paths, true, true, true, false); err != nil {
			return "", err
		}
		token, tokenErr := readPrivateToken(paths.token)
		if tokenErr != nil {
			return "", model.NewError(model.CodeAmbiguous, "host AWS helper token evidence is unavailable; preserving it", tokenErr)
		}
		response, controlErr := manager.control(ctx, paths, leappMirrorCommand{Version: 1, Action: "status", Identity: identity, SpecDigest: digest, Token: token})
		if controlErr != nil {
			return "", model.NewError(model.CodeAmbiguous, "host AWS helper cannot prove its live authority; preserving it", controlErr)
		}
		if err := validateLeappMirrorResponse(response, ledger, "running"); err != nil {
			return "", model.NewError(model.CodeAmbiguous, "host AWS helper returned contradictory ownership evidence; preserving it", err)
		}
		if err := verifyLeappMirrorSnapshot(paths.mirror); err != nil {
			return "", model.NewError(model.CodeAmbiguous, "host AWS publication is unsafe or incomplete; preserving it", err)
		}
		return paths.mirror, nil
	}
	failure, failed, err := loadPrivateJSON[leappMirrorFailure](paths.failure, MaxControlBytes)
	if err != nil {
		return "", model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence is unsafe or corrupt; preserving it", err)
	}
	if failed && (failure.Version != 1 || failure.Identity != identity || failure.SpecDigest != digest || !validLeappMirrorFailure(failure.Failure)) {
		return "", model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence differs from the requested workspace; preserving it", nil)
	}
	if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
		return "", err
	}
	if failed {
		if err := removePrivateRegular(paths.failure); err != nil {
			return "", model.Wrap(model.CodeInternal, "clear exact host AWS helper failure evidence", err)
		}
	}
	if err := rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, false); err != nil {
		return "", err
	}
	if err := ensureHostAWSPublication(paths.mirror, false); err != nil {
		return "", model.NewError(model.CodeAmbiguous, "prepare host AWS publication", err)
	}
	if manager.launchOverride != nil {
		return manager.launchOverride(ctx, paths, identity, spec, digest)
	}
	return manager.launch(ctx, paths, identity, spec, digest)
}

func (manager *ProductionHostAWSWorkspaceManager) Disable(ctx context.Context, identity LeaseIdentity) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "host AWS workspace disable context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid host AWS workspace identity", err)
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
	if err := manager.stopHelperLocked(ctx, paths, identity); err != nil {
		return err
	}
	if err := ensureHostAWSPublication(paths.mirror, true); err != nil {
		return model.NewError(model.CodeAmbiguous, "publish empty disabled host AWS channel", err)
	}
	return nil
}

func (manager *ProductionHostAWSWorkspaceManager) Remove(ctx context.Context, identity LeaseIdentity) error {
	if err := manager.Disable(ctx, identity); err != nil {
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
	if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
		return err
	}
	if _, found, err := loadPrivateJSON[leappMirrorFailure](paths.failure, MaxControlBytes); err != nil || found {
		return model.NewError(model.CodeAmbiguous, "host AWS removal found unexpected failure evidence; preserving it", err)
	}
	if err := rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, false); err != nil {
		return err
	}
	if err := removeLeappMirrorDirectory(paths.mirror); err != nil {
		return model.NewError(model.CodeAmbiguous, "remove exact host AWS publication", err)
	}
	return removeExactLeappMirrorRunDirectory(paths.run)
}

func (manager *ProductionHostAWSWorkspaceManager) prepareLocked(paths leappMirrorPaths, identity LeaseIdentity) error {
	if _, found, err := loadPrivateJSON[leappMirrorLedger](paths.ledger, MaxControlBytes); err != nil || found {
		return model.NewError(model.CodeAmbiguous, "host AWS prepare found active or unsafe helper evidence; preserving it", err)
	}
	if failure, found, err := loadPrivateJSON[leappMirrorFailure](paths.failure, MaxControlBytes); err != nil || found {
		if err == nil && (failure.Version != 1 || failure.Identity != identity || !validLeappMirrorFailure(failure.Failure)) {
			err = errors.New("contradictory host AWS failure ownership")
		}
		return model.NewError(model.CodeAmbiguous, "host AWS prepare found helper failure evidence; preserving it", err)
	}
	if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
		return err
	}
	if err := rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, false); err != nil {
		return err
	}
	if err := ensureHostAWSPublication(paths.mirror, false); err != nil {
		return model.NewError(model.CodeAmbiguous, "prepare host AWS publication", err)
	}
	empty, err := hostAWSPublicationEmpty(paths.mirror)
	if err != nil || !empty {
		if err == nil {
			err = errors.New("existing host AWS publication is not empty")
		}
		return model.NewError(model.CodeAmbiguous, "host AWS publication has unexpected content; preserving it", err)
	}
	return nil
}

func (manager *ProductionHostAWSWorkspaceManager) stopHelperLocked(ctx context.Context, paths leappMirrorPaths, identity LeaseIdentity) error {
	ledger, found, err := loadPrivateJSON[leappMirrorLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return model.NewError(model.CodeAmbiguous, "host AWS helper ledger is unsafe or corrupt; preserving it", err)
	}
	if !found {
		failure, failed, failureErr := loadPrivateJSON[leappMirrorFailure](paths.failure, MaxControlBytes)
		if failureErr != nil {
			return model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence is unsafe or corrupt; preserving it", failureErr)
		}
		if failed && (failure.Version != 1 || failure.Identity != identity || !validLeappMirrorFailure(failure.Failure)) {
			return model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence has contradictory ownership; preserving it", nil)
		}
		if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
			return err
		}
		if failed {
			if err := removePrivateRegular(paths.failure); err != nil {
				return model.Wrap(model.CodeInternal, "remove exact host AWS helper failure evidence", err)
			}
		}
		if err := rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, false); err != nil {
			return err
		}
		return nil
	}
	if _, err := os.Lstat(paths.failure); err == nil {
		return model.NewError(model.CodeAmbiguous, "active host AWS helper has contradictory failure evidence; preserving it", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := manager.verifyExecutable(); err != nil {
		return err
	}
	if ledger.Version != 1 || ledger.Identity != identity || ledger.PID <= 0 || ledger.Executable != manager.executable {
		return model.NewError(model.CodeAmbiguous, "host AWS helper ownership evidence is contradictory; preserving it", nil)
	}
	if err := rejectUnexpectedHostAWSRunEvidence(paths, true, true, true, false); err != nil {
		return err
	}
	token, err := readPrivateToken(paths.token)
	if err != nil {
		return model.NewError(model.CodeAmbiguous, "host AWS helper token evidence is unavailable; preserving it", err)
	}
	response, err := manager.control(ctx, paths, leappMirrorCommand{Version: 1, Action: "stop", Identity: identity, SpecDigest: ledger.SpecDigest, Token: token})
	if err != nil {
		if recoveryErr := manager.recoverDeadLeappMirror(paths, ledger); recoveryErr != nil {
			return model.NewError(model.CodeAmbiguous, "host AWS helper did not authenticate stop and absence is unproven; preserving it", errors.Join(err, recoveryErr))
		}
		return nil
	}
	if err := validateLeappMirrorResponse(response, ledger, "stopped"); err != nil {
		return model.NewError(model.CodeAmbiguous, "host AWS helper returned contradictory stop evidence; preserving it", err)
	}
	if err := manager.waitForLeappMirrorArtifactsAbsent(ctx, paths); err != nil {
		return model.NewError(model.CodeUnavailable, "host AWS helper did not finish exact cleanup before the deadline", err)
	}
	return nil
}
func (manager *ProductionHostAWSWorkspaceManager) recoverDeadLeappMirror(paths leappMirrorPaths, ledger leappMirrorLedger) error {
	if ledger.Executable != manager.executable || ledger.PID <= 0 {
		return errors.New("host AWS dead-helper ownership evidence is contradictory")
	}
	verifySocket := manager.verifySocket
	if verifySocket == nil {
		verifySocket = verifyPrivateSocket
	}
	if err := verifySocket(paths.socket); err != nil {
		return errors.New("host AWS dead-helper socket evidence is unavailable")
	}
	processAlive := manager.processAlive
	if processAlive == nil {
		processAlive = leappMirrorProcessAlive
	}
	alive, err := processAlive(ledger.PID)
	if err != nil || alive {
		return errors.New("host AWS helper absence is unproven")
	}
	if err := removePrivateSocket(paths.socket); err != nil {
		return err
	}
	if err := removePrivateRegular(paths.token); err != nil {
		return err
	}
	if err := removePrivateRegular(paths.ledger); err != nil {
		return err
	}
	if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
		return err
	}
	return rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, false)
}

func leappMirrorProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func (manager *ProductionHostAWSWorkspaceManager) Status(ctx context.Context, identity LeaseIdentity) (HostAWSMirrorStatus, error) {
	if ctx == nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeInvalidInput, "host AWS workspace status context is nil", nil)
	}
	if err := identity.Validate(); err != nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeInvalidInput, "invalid host AWS workspace identity", err)
	}
	paths, exists, err := manager.existingPaths(identity)
	if err != nil || !exists {
		return HostAWSMirrorStatus{State: "disabled"}, err
	}
	unlock, err := acquireLeaseLock(ctx, paths.lock)
	if err != nil {
		return HostAWSMirrorStatus{}, err
	}
	defer unlock()
	ledger, found, err := loadPrivateJSON[leappMirrorLedger](paths.ledger, MaxControlBytes)
	if err != nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS helper ledger is unsafe or corrupt", err)
	}
	if !found {
		failure, failed, failureErr := loadPrivateJSON[leappMirrorFailure](paths.failure, MaxControlBytes)
		if failureErr != nil {
			return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence is unsafe or corrupt", failureErr)
		}
		if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
			return HostAWSMirrorStatus{}, err
		}
		if err := rejectUnexpectedHostAWSRunEvidence(paths, false, false, false, failed); err != nil {
			return HostAWSMirrorStatus{}, err
		}
		if err := verifyLeappMirrorSnapshot(paths.mirror); err != nil {
			if errors.Is(err, os.ErrNotExist) && !failed {
				return HostAWSMirrorStatus{State: "disabled"}, nil
			}
			return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS publication is unsafe or incomplete", err)
		}
		if failed {
			if failure.Version != 1 || failure.Identity != identity || !validLeappMirrorFailure(failure.Failure) {
				return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS helper failure evidence has contradictory ownership", nil)
			}
			return HostAWSMirrorStatus{State: "degraded", Failure: failure.Failure}, nil
		}
		empty, emptyErr := hostAWSPublicationEmpty(paths.mirror)
		if emptyErr != nil {
			return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "inspect host AWS publication", emptyErr)
		}
		if empty {
			return HostAWSMirrorStatus{State: "disabled"}, nil
		}
		return HostAWSMirrorStatus{State: "stopped"}, nil
	}
	if ledger.Version != 1 || ledger.Identity != identity || ledger.Executable != manager.executable {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS helper ownership evidence is contradictory", nil)
	}
	if _, err := os.Lstat(paths.failure); err == nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "active host AWS helper has contradictory failure evidence", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return HostAWSMirrorStatus{}, err
	}
	if err := rejectUnexpectedHostAWSRunEvidence(paths, true, true, true, false); err != nil {
		return HostAWSMirrorStatus{}, err
	}
	token, err := readPrivateToken(paths.token)
	if err != nil {
		return HostAWSMirrorStatus{State: "stopped"}, nil
	}
	response, err := manager.control(ctx, paths, leappMirrorCommand{Version: 1, Action: "status", Identity: identity, SpecDigest: ledger.SpecDigest, Token: token})
	if err != nil {
		return HostAWSMirrorStatus{State: "stopped"}, nil
	}
	if err := validateLeappMirrorResponse(response, ledger, "running"); err != nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS helper returned contradictory ownership evidence", err)
	}
	if err := verifyLeappMirrorSnapshot(paths.mirror); err != nil {
		return HostAWSMirrorStatus{}, model.NewError(model.CodeAmbiguous, "host AWS publication is unsafe or incomplete", err)
	}
	return HostAWSMirrorStatus{State: "current"}, nil
}
func configureLeappMirrorCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func (manager *ProductionHostAWSWorkspaceManager) launch(ctx context.Context, paths leappMirrorPaths, identity LeaseIdentity, spec leappMirrorSpec, digest string) (string, error) {
	if err := rejectUnexpectedHostAWSHelperEvidence(paths); err != nil {
		return "", err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", model.Wrap(model.CodeInternal, "generate Leapp mirror control token", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	if err := atomicWritePrivate(paths.token, []byte(token+"\n")); err != nil {
		return "", model.Wrap(model.CodeInternal, "write Leapp mirror control token", err)
	}
	cleanupToken := true
	defer func() {
		if cleanupToken {
			_ = removePrivateRegular(paths.token)
		}
	}()
	commandInput := leappMirrorCommand{Version: 1, Action: "start", StateRoot: manager.stateRoot, Identity: identity, Spec: spec, SpecDigest: digest, Token: token, Executable: manager.executable}
	input, err := json.Marshal(commandInput)
	if err != nil || len(input) > MaxHelperInputBytes {
		return "", model.NewError(model.CodeInternal, "encode bounded Leapp mirror helper input", err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return "", model.Wrap(model.CodeInternal, "create Leapp mirror readiness pipe", err)
	}
	defer readyReader.Close()
	acceptanceReader, acceptanceWriter, err := os.Pipe()
	if err != nil {
		readyReader.Close()
		readyWriter.Close()
		return "", model.Wrap(model.CodeInternal, "create Leapp mirror acceptance pipe", err)
	}
	defer acceptanceReader.Close()
	defer acceptanceWriter.Close()
	command := exec.Command(manager.executable.Path, "__dsx_host_aws_mirror_v1")
	command.Dir = paths.run
	command.Stdin = bytes.NewReader(input)
	command.Stdout = nil
	command.Stderr = nil
	command.ExtraFiles = []*os.File{readyWriter, acceptanceReader}
	configureLeappMirrorCommand(command)
	if err := command.Start(); err != nil {
		readyWriter.Close()
		return "", model.Wrap(model.CodeUnavailable, "start Leapp mirror helper", err)
	}
	cleanupToken = false
	acceptanceReader.Close()
	readyWriter.Close()
	failUnaccepted := func(cause error) error {
		_ = readyReader.Close()
		_ = acceptanceWriter.Close()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), manager.stopWait)
		defer cancelCleanup()
		if err := manager.waitForLeappMirrorArtifactsAbsent(cleanupCtx, paths); err != nil {
			return model.NewError(model.CodeAmbiguous, "Leapp mirror unaccepted readiness cleanup is unproven; preserving ownership evidence", errors.Join(cause, err))
		}
		return cause
	}
	defer command.Process.Release()
	readyContext, cancel := context.WithTimeout(ctx, manager.readyWait)
	defer cancel()
	result := make(chan struct {
		ready leappMirrorResponse
		err   error
	}, 1)
	go func() {
		var ready leappMirrorResponse
		err := decodeBoundedJSON(readyReader, MaxControlBytes, &ready)
		result <- struct {
			ready leappMirrorResponse
			err   error
		}{ready: ready, err: err}
	}()
	var ready leappMirrorResponse
	select {
	case <-readyContext.Done():
		return "", failUnaccepted(model.Wrap(model.CodeUnavailable, "wait for Leapp mirror helper readiness", readyContext.Err()))
	case decoded := <-result:
		if decoded.err != nil {
			return "", failUnaccepted(model.Wrap(model.CodeUnavailable, "read Leapp mirror helper readiness", decoded.err))
		}
		ready = decoded.ready
	}
	if ready.Version == 1 && ready.State == "error" && ready.Identity == identity && ready.SpecDigest == digest && ready.PID == command.Process.Pid && ready.Executable == manager.executable && validLeappMirrorFailure(ready.Failure) {
		return "", failUnaccepted(model.NewError(model.CodeUnavailable, "host AWS helper could not publish its first filtered generation: "+ready.Failure, nil))
	}
	if ready.Version != 1 || ready.State != "ready" || ready.Identity != identity || ready.SpecDigest != digest || ready.PID != command.Process.Pid || ready.Executable != manager.executable {
		return "", failUnaccepted(model.NewError(model.CodeAmbiguous, "Leapp mirror helper readiness evidence differs from the requested authority", nil))
	}
	if err := manager.validateReadyLeappMirrorArtifacts(paths, identity, spec, digest, ready); err != nil {
		return "", manager.failAcceptedLeappMirrorLaunch(ctx, paths, identity, digest, token, ready, acceptanceWriter, err)
	}
	if !writeLeappMirrorAcceptance(acceptanceWriter) {
		return "", model.NewError(model.CodeAmbiguous, "Leapp mirror helper readiness acceptance failed; preserving ownership evidence", nil)
	}
	return paths.mirror, nil
}
func (manager *ProductionHostAWSWorkspaceManager) validateReadyLeappMirrorArtifacts(paths leappMirrorPaths, identity LeaseIdentity, spec leappMirrorSpec, digest string, ready leappMirrorResponse) error {
	loadLedger := manager.loadReadyLedger
	if loadLedger == nil {
		loadLedger = func(path string) (leappMirrorLedger, bool, error) {
			return loadPrivateJSON[leappMirrorLedger](path, MaxControlBytes)
		}
	}
	ledger, found, err := loadLedger(paths.ledger)
	if err != nil || !found {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror helper did not publish a valid private ledger", err)
	}
	if err := validateLeappMirrorLedger(ledger, identity, spec, digest, manager.executable); err != nil || ledger.PID != ready.PID {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror helper ledger differs from readiness evidence", err)
	}
	verifySnapshot := manager.verifySnapshot
	if verifySnapshot == nil {
		verifySnapshot = verifyLeappMirrorSnapshot
	}
	if err := verifySnapshot(paths.mirror); err != nil {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror helper did not publish a complete safe snapshot", err)
	}
	return nil
}

func (manager *ProductionHostAWSWorkspaceManager) failAcceptedLeappMirrorLaunch(ctx context.Context, paths leappMirrorPaths, identity LeaseIdentity, digest, token string, ready leappMirrorResponse, acceptance io.Writer, cause error) error {
	if !writeLeappMirrorAcceptance(acceptance) {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror launch failed before readiness acceptance; preserving ownership evidence", cause)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.stopWait)
	defer cancel()
	if err := manager.stopAcceptedLeappMirrorLaunch(cleanupCtx, paths, identity, digest, token, ready); err != nil {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror launch cleanup could not prove exact ownership; preserving evidence", errors.Join(cause, err))
	}
	return cause
}

func (manager *ProductionHostAWSWorkspaceManager) stopAcceptedLeappMirrorLaunch(ctx context.Context, paths leappMirrorPaths, identity LeaseIdentity, digest, token string, ready leappMirrorResponse) error {
	response, err := manager.control(ctx, paths, leappMirrorCommand{Version: 1, Action: "stop", Identity: identity, SpecDigest: digest, Token: token})
	if err != nil {
		return err
	}
	if response.Version != 1 || response.State != "stopped" || response.Identity != ready.Identity || response.SpecDigest != ready.SpecDigest || response.PID != ready.PID || response.Executable != ready.Executable {
		return errors.New("invalid stopped Leapp mirror response after accepted readiness")
	}
	if err := manager.waitForLeappMirrorArtifactsAbsent(ctx, paths); err != nil {
		return err
	}
	return nil
}

func (manager *ProductionHostAWSWorkspaceManager) waitForLeappMirrorArtifactsAbsent(ctx context.Context, paths leappMirrorPaths) error {
	artifactsAbsent := manager.artifactsAbsent
	if artifactsAbsent == nil {
		artifactsAbsent = exactLeappMirrorHelperArtifactsAbsent
	}
	deadline := time.Now().Add(manager.stopWait)
	for {
		absent, absenceErr := artifactsAbsent(paths)
		if absenceErr != nil {
			return absenceErr
		}
		if absent {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("Leapp mirror did not clean exact artifacts")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (manager *ProductionHostAWSWorkspaceManager) control(ctx context.Context, paths leappMirrorPaths, request leappMirrorCommand) (leappMirrorResponse, error) {
	if manager.controlOverride != nil {
		return manager.controlOverride(ctx, paths, request)
	}
	var response leappMirrorResponse
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxControlBytes {
		return response, errors.New("invalid bounded Leapp mirror control request")
	}
	controlContext, cancel := context.WithTimeout(ctx, manager.stopWait)
	defer cancel()
	output := &boundedOutput{maximum: MaxControlBytes}
	command := exec.CommandContext(controlContext, manager.executable.Path, "__dsx_host_aws_mirror_v1")
	command.Dir = paths.run
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

func (manager *ProductionHostAWSWorkspaceManager) ensurePaths(identity LeaseIdentity) (leappMirrorPaths, error) {
	if err := verifyPrivateDirectory(manager.stateRoot); err != nil {
		return leappMirrorPaths{}, model.Wrap(model.CodeUnavailable, "verify Leapp mirror state root", err)
	}
	root, err := ensurePrivateChildDirectory(manager.stateRoot, hostAWSWorkspaceDirectoryName)
	if err != nil {
		return leappMirrorPaths{}, err
	}
	project, err := ensurePrivateChildDirectory(root, string(identity.ProjectID))
	if err != nil {
		return leappMirrorPaths{}, err
	}
	workspace, err := ensurePrivateChildDirectory(project, string(identity.Workspace))
	if err != nil {
		return leappMirrorPaths{}, err
	}
	run, err := ensurePrivateChildDirectory(workspace, string(identity.RunID))
	if err != nil {
		return leappMirrorPaths{}, err
	}
	return makeLeappMirrorPaths(root, project, workspace, run), nil
}

func (manager *ProductionHostAWSWorkspaceManager) existingPaths(identity LeaseIdentity) (leappMirrorPaths, bool, error) {
	root := filepath.Join(manager.stateRoot, hostAWSWorkspaceDirectoryName)
	project := filepath.Join(root, string(identity.ProjectID))
	workspace := filepath.Join(project, string(identity.Workspace))
	run := filepath.Join(workspace, string(identity.RunID))
	for _, path := range []string{manager.stateRoot, root, project, workspace} {
		if err := verifyPrivateDirectory(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return leappMirrorPaths{}, false, nil
			}
			return leappMirrorPaths{}, false, model.NewError(model.CodeAmbiguous, "Leapp mirror state ancestry is unsafe", err)
		}
	}
	if err := verifyPrivateDirectory(run); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return leappMirrorPaths{}, false, nil
		}
		return leappMirrorPaths{}, false, model.NewError(model.CodeAmbiguous, "Leapp mirror run directory is unsafe", err)
	}
	return makeLeappMirrorPaths(root, project, workspace, run), true, nil
}

func makeLeappMirrorPaths(root, project, workspace, run string) leappMirrorPaths {
	return leappMirrorPaths{root: root, project: project, workspace: workspace, run: run, ledger: filepath.Join(run, leappMirrorLedgerName), token: filepath.Join(run, leappMirrorTokenName), failure: filepath.Join(run, leappMirrorFailureName), socket: filepath.Join(run, leappMirrorSocketName), lock: filepath.Join(workspace, leappMirrorLockName), mirror: filepath.Join(run, hostAWSWorkspaceDataName)}
}

func (manager *ProductionHostAWSWorkspaceManager) verifyExecutable() error {
	current, err := canonicalExecutableIdentity(manager.executable.Path)
	if err != nil || current != manager.executable {
		return model.NewError(model.CodeAmbiguous, "dsx Leapp mirror executable identity changed", err)
	}
	return nil
}

func validateLeappMirrorLedger(ledger leappMirrorLedger, identity LeaseIdentity, spec leappMirrorSpec, digest string, executable executableIdentity) error {
	if ledger.Version != 1 || ledger.Identity != identity || ledger.Spec != spec || ledger.SpecDigest != digest || ledger.PID <= 0 || ledger.ProcessStartedAt.IsZero() || ledger.Executable != executable {
		return errors.New("invalid Leapp mirror ledger identity")
	}
	return nil
}

func validateLeappMirrorResponse(response leappMirrorResponse, ledger leappMirrorLedger, state string) error {
	if response.Version != 1 || response.State != state || response.Identity != ledger.Identity || response.SpecDigest != ledger.SpecDigest || response.PID != ledger.PID || response.Executable != ledger.Executable {
		return errors.New("invalid Leapp mirror control response")
	}
	return nil
}

func validLeappMirrorFailure(value string) bool {
	switch value {
	case "source_identity_changed", "source_unsafe", "source_oversized", "source_unavailable", "control_failed", "startup_failed":
		return true
	default:
		return false
	}
}

func exactLeappMirrorHelperArtifactsAbsent(paths leappMirrorPaths) (bool, error) {
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

func rejectUnexpectedHostAWSHelperEvidence(paths leappMirrorPaths) error {
	for _, path := range []string{paths.token, paths.socket} {
		if _, err := os.Lstat(path); err == nil {
			return model.NewError(model.CodeAmbiguous, "host AWS workspace has incomplete helper ownership evidence; preserving it", nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func rejectUnexpectedHostAWSRunEvidence(paths leappMirrorPaths, allowLedger, allowToken, allowSocket, allowFailure bool) error {
	entries, err := os.ReadDir(paths.run)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		allowed := entry.Name() == filepath.Base(paths.mirror) ||
			allowLedger && entry.Name() == filepath.Base(paths.ledger) ||
			allowToken && entry.Name() == filepath.Base(paths.token) ||
			allowSocket && entry.Name() == filepath.Base(paths.socket) ||
			allowFailure && entry.Name() == filepath.Base(paths.failure)
		if !allowed {
			return model.NewError(model.CodeAmbiguous, "host AWS workspace retained unexpected evidence; preserving it", nil)
		}
	}
	return nil
}
func rejectUnexpectedHostAWSPublicationEvidence(mirror string) error {
	entries, err := os.ReadDir(mirror)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		known := entry.Name() == leappMirrorCurrentName && entry.Type()&os.ModeSymlink != 0 ||
			strings.HasPrefix(entry.Name(), leappMirrorGenerationPrefix) && entry.IsDir() ||
			strings.HasPrefix(entry.Name(), leappMirrorWritePrefix) && entry.Type().IsRegular() ||
			strings.HasPrefix(entry.Name(), leappMirrorCurrentPrefix) && (entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0)
		if !known {
			return model.NewError(model.CodeAmbiguous, "host AWS publication retained unexpected evidence; preserving it", nil)
		}
	}
	return nil
}

func ensureHostAWSPublication(mirror string, publishEmpty bool) error {
	info, err := os.Lstat(mirror)
	if errors.Is(err, os.ErrNotExist) {
		created, createErr := ensurePrivateChildDirectory(filepath.Dir(mirror), filepath.Base(mirror))
		if createErr != nil {
			return createErr
		}
		if created != mirror {
			return errors.New("host AWS publication path changed during creation")
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("host AWS publication path is not a private directory")
	} else if err := verifyPrivateDirectory(mirror); err != nil {
		return err
	}
	if err := rejectUnexpectedHostAWSPublicationEvidence(mirror); err != nil {
		return err
	}
	generation, err := currentLeappMirrorGeneration(mirror)
	if err != nil {
		return err
	}
	if generation == "" {
		entries, readErr := os.ReadDir(mirror)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return errors.New("host AWS publication has incomplete ownership evidence")
		}
		return publishLeappMirrorGeneration(mirror, HostAWSDirectorySnapshot{}, nil)
	}
	if !publishEmpty {
		return verifyLeappMirrorSnapshot(mirror)
	}
	empty, err := hostAWSPublicationEmpty(mirror)
	if err != nil || empty {
		return err
	}
	return publishLeappMirrorGeneration(mirror, HostAWSDirectorySnapshot{}, nil)
}

func hostAWSPublicationEmpty(mirror string) (bool, error) {
	if err := verifyLeappMirrorSnapshot(mirror); err != nil {
		return false, err
	}
	generation, err := currentLeappMirrorGeneration(mirror)
	if err != nil {
		return false, err
	}
	for _, name := range []string{hostAWSConfigFile, hostAWSCredentialsFile} {
		info, err := os.Lstat(filepath.Join(generation, name))
		if err != nil {
			return false, err
		}
		if info.Size() != 0 {
			return false, nil
		}
	}
	return true, nil
}

func verifyLeappMirrorSnapshot(path string) error {
	if err := verifyPrivateDirectory(path); err != nil {
		return err
	}
	generation, err := currentLeappMirrorGeneration(path)
	if err != nil {
		return err
	}
	if generation == "" {
		return errors.New("Leapp mirror has no current generation")
	}
	return verifyLeappMirrorGeneration(generation)
}

func currentLeappMirrorGeneration(mirror string) (string, error) {
	current := filepath.Join(mirror, leappMirrorCurrentName)
	info, err := os.Lstat(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("Leapp mirror current pointer is not a symlink")
	}
	target, err := os.Readlink(current)
	if err != nil || target == "" || filepath.IsAbs(target) || filepath.Base(target) != target || !strings.HasPrefix(target, leappMirrorGenerationPrefix) {
		return "", errors.New("Leapp mirror current pointer target is invalid")
	}
	generation := filepath.Join(mirror, target)
	if !pathContains(mirror, generation) {
		return "", errors.New("Leapp mirror current pointer escapes its mirror")
	}
	if err := verifyLeappMirrorGeneration(generation); err != nil {
		return "", err
	}
	return generation, nil
}

func verifyLeappMirrorGeneration(path string) error {
	if err := verifyPrivateDirectory(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return errors.New("host AWS publication generation has unexpected artifacts")
	}
	for _, entry := range entries {
		if entry.Name() != hostAWSConfigFile && entry.Name() != hostAWSCredentialsFile {
			return errors.New("host AWS publication generation has unexpected artifacts")
		}
	}
	for _, name := range []string{hostAWSConfigFile, hostAWSCredentialsFile} {
		file := filepath.Join(path, name)
		info, err := os.Lstat(file)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() < 0 || info.Size() > MaxHostAWSFileBytes || !ok || stat.Uid != uint32(os.Geteuid()) {
			return errors.New("Leapp mirror file type, mode, owner, or size is invalid")
		}
	}
	return nil
}

func removeLeappMirrorDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("refusing to remove unsafe Leapp mirror directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("refusing to remove unowned Leapp mirror directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if _, err := currentLeappMirrorGeneration(path); err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		switch {
		case entry.Name() == leappMirrorCurrentName && entry.Type()&os.ModeSymlink != 0:
			if err := os.Remove(entryPath); err != nil {
				return err
			}
		case strings.HasPrefix(entry.Name(), leappMirrorGenerationPrefix) && entry.IsDir():
			if err := removeStagedLeappMirrorGeneration(entryPath); err != nil {
				return err
			}
		case strings.HasPrefix(entry.Name(), leappMirrorWritePrefix) && entry.Type().IsRegular():
			if err := removeLeappMirrorStagingFile(entryPath, false); err != nil {
				return err
			}
		case strings.HasPrefix(entry.Name(), leappMirrorCurrentPrefix):
			if err := removeLeappMirrorCurrentStagingArtifact(path, entryPath); err != nil {
				return err
			}
		default:
			return errors.New("refusing to remove unexpected Leapp mirror artifact")
		}
	}
	return os.Remove(path)
}

func removeLeappMirrorGeneration(path string) error {
	if err := verifyLeappMirrorGeneration(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return errors.New("refusing to remove incomplete Leapp mirror generation")
	}
	for _, entry := range entries {
		if entry.Name() != hostAWSConfigFile && entry.Name() != hostAWSCredentialsFile {
			return errors.New("refusing to remove unexpected Leapp mirror generation artifact")
		}
		if err := removeLeappMirrorFile(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func removeStagedLeappMirrorGeneration(path string) error {
	if err := verifyPrivateDirectory(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != hostAWSConfigFile && entry.Name() != hostAWSCredentialsFile {
			return errors.New("refusing to remove unexpected staged Leapp mirror artifact")
		}
		if err := removeLeappMirrorFile(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func removeLeappMirrorFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("refusing to remove unsafe Leapp mirror file")
	}
	return os.Remove(path)
}

func removeLeappMirrorStagingFile(path string, requireEmpty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400 || info.Size() < 0 || info.Size() > MaxHostAWSFileBytes || requireEmpty && info.Size() != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("refusing to remove unsafe Leapp mirror staging file")
	}
	return os.Remove(path)
}

func removeLeappMirrorCurrentStagingArtifact(mirror, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return removeLeappMirrorStagingFile(path, true)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("refusing to remove unsafe Leapp mirror pointer staging artifact")
	}
	target, err := os.Readlink(path)
	if err != nil || filepath.IsAbs(target) || filepath.Base(target) != target || !strings.HasPrefix(target, leappMirrorGenerationPrefix) {
		return errors.New("refusing to remove unsafe Leapp mirror pointer staging symlink")
	}
	generation := filepath.Join(mirror, target)
	if !pathContains(mirror, generation) {
		return errors.New("refusing to remove escaping Leapp mirror pointer staging symlink")
	}
	if err := verifyPrivateDirectory(generation); err != nil {
		return err
	}
	return os.Remove(path)
}

func removeExactLeappMirrorRunDirectory(path string) error {
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return model.NewError(model.CodeAmbiguous, "Leapp mirror run directory retained unexpected evidence; preserving it", err)
	}
	return nil
}
