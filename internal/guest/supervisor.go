package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/srimajji/dsx/internal/guestproto"
)

const (
	DefaultSocketPath     = "/run/dsx/control.sock"
	maxIdempotencyEntries = 1024
	defaultShutdownGrace  = 5 * time.Second
)

type Options struct {
	Version       string
	InstanceID    string
	ChildUID      uint32
	ChildGID      uint32
	Output        io.Writer
	ShutdownGrace time.Duration
}

type Supervisor struct {
	mu               sync.Mutex
	opMu             sync.Mutex
	version          string
	instanceID       string
	output           *asyncOutput
	shutdownGrace    time.Duration
	childUID         uint32
	childGID         uint32
	generation       uint64
	failed           bool
	shuttingDown     bool
	processes        map[string]*process
	order            []string
	idempotency      map[string]idempotencyEntry
	idempotencyOrder []string
	done             chan struct{}
	doneOnce         sync.Once
}

type process struct {
	spec         guestproto.ProcessSpec
	generation   uint64
	state        guestproto.ProcessState
	ready        bool
	exit         *guestproto.ExitStatus
	failure      string
	log          *processLog
	cmd          *exec.Cmd
	terminal     *os.File
	terminalDone chan struct{}
	readyCh      chan struct{}
	doneCh       chan struct{}
	controlDone  chan struct{}
	readyOnce    sync.Once
	doneOnce     sync.Once
}

type idempotencyEntry struct {
	fingerprint [sha256.Size]byte
	response    guestproto.Response
}

type codedError struct {
	code       guestproto.ErrorCode
	message    string
	generation *uint64
	cause      error
}

func (err *codedError) Error() string {
	if err.cause == nil {
		return err.message
	}
	return err.message + ": " + err.cause.Error()
}

func NewSupervisor(options Options) (*Supervisor, error) {
	if strings.TrimSpace(options.Version) == "" || options.Version != strings.TrimSpace(options.Version) || len(options.Version) > guestproto.MaxStringBytes {
		return nil, errors.New("guest version must be non-empty and bounded")
	}
	if options.ChildUID == 0 || options.ChildGID == 0 {
		return nil, errors.New("non-root child UID and GID are required")
	}
	if os.Geteuid() != 0 && (options.ChildUID != uint32(os.Geteuid()) || options.ChildGID != uint32(os.Getegid())) {
		return nil, errors.New("changing child identity requires root")
	}
	instanceID := options.InstanceID
	if instanceID == "" {
		var err error
		instanceID, err = newUUID()
		if err != nil {
			return nil, fmt.Errorf("generate instance ID: %w", err)
		}
	}
	if !guestproto.ValidUUID(instanceID) {
		return nil, errors.New("instance ID must be a canonical UUID")
	}
	grace := options.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	if err := enableChildSubreaper(); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	supervisor := &Supervisor{
		version:       options.Version,
		instanceID:    instanceID,
		output:        newAsyncOutput(options.Output),
		shutdownGrace: grace,
		childUID:      options.ChildUID,
		childGID:      options.ChildGID,
		processes:     make(map[string]*process),
		idempotency:   make(map[string]idempotencyEntry),
		done:          make(chan struct{}),
	}
	startOrphanReaper(supervisor.done, supervisor.directChildPIDs)
	return supervisor, nil
}

func (supervisor *Supervisor) Done() <-chan struct{} { return supervisor.done }

func (supervisor *Supervisor) ServerIdentity() guestproto.Server {
	return guestproto.Server{InstanceID: supervisor.instanceID, Version: supervisor.version}
}

func (supervisor *Supervisor) Handle(ctx context.Context, request guestproto.Request) guestproto.Response {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return supervisor.errorResponse(request.RequestID, guestproto.ErrorCodeOf(err), "invalid request", nil)
	}
	requestContext, cancel := context.WithTimeout(ctx, time.Duration(request.DeadlineMS)*time.Millisecond)
	defer cancel()
	ctx = requestContext
	if isMutation(request.Operation) {
		supervisor.opMu.Lock()
		defer supervisor.opMu.Unlock()
		fingerprint := requestFingerprint(request)
		supervisor.mu.Lock()
		if cached, found := supervisor.idempotency[request.IdempotencyKey]; found {
			supervisor.mu.Unlock()
			if cached.fingerprint != fingerprint {
				generation := supervisor.currentGeneration()
				return supervisor.errorResponse(request.RequestID, guestproto.CodeIdempotencyConflict, "idempotency key was already used for a different request", &generation)
			}
			return cloneResponse(cached.response)
		}
		supervisor.mu.Unlock()
		response := supervisor.dispatch(ctx, request)
		supervisor.storeIdempotency(request.IdempotencyKey, fingerprint, response)
		return response
	}
	return supervisor.dispatch(ctx, request)
}

func (supervisor *Supervisor) dispatch(ctx context.Context, request guestproto.Request) guestproto.Response {
	if err := ctx.Err(); err != nil {
		return supervisor.errorResponse(request.RequestID, guestproto.CodeDeadlineExceeded, "operation deadline exceeded", nil)
	}
	var result any
	var err error
	switch request.Operation {
	case guestproto.OperationPing:
		if decodeErr := decodeEmptyParams(request.Params); decodeErr != nil {
			err = decodeErr
		} else {
			result = map[string]any{}
		}
	case guestproto.OperationStatus:
		if decodeErr := decodeEmptyParams(request.Params); decodeErr != nil {
			err = decodeErr
		} else {
			result, err = boundedStatusJSON(supervisor.Status())
		}
	case guestproto.OperationStart:
		var params guestproto.StartParams
		if decodeErr := guestproto.DecodeParams(request.Params, &params); decodeErr != nil {
			err = &codedError{code: guestproto.CodeInvalidRequest, message: "invalid start parameters", cause: decodeErr}
			break
		}
		var generation uint64
		generation, err = supervisor.Start(ctx, *request.IfGeneration, params)
		result = guestproto.StartResult{Generation: generation}
	case guestproto.OperationSignal:
		var params guestproto.SignalParams
		if decodeErr := guestproto.DecodeParams(request.Params, &params); decodeErr != nil {
			err = &codedError{code: guestproto.CodeInvalidRequest, message: "invalid signal parameters", cause: decodeErr}
			break
		}
		err = supervisor.Signal(request.Target, *request.IfGeneration, params.Signal)
		if err == nil {
			result = map[string]any{}
		}
	case guestproto.OperationResize:
		var params guestproto.ResizeParams
		if decodeErr := guestproto.DecodeParams(request.Params, &params); decodeErr != nil || params.Columns == 0 || params.Rows == 0 {
			err = &codedError{code: guestproto.CodeInvalidRequest, message: "invalid resize parameters", cause: decodeErr}
		} else {
			err = supervisor.Resize(request.Target, *request.IfGeneration, params.Columns, params.Rows)
		}
	case guestproto.OperationWait:
		if decodeErr := decodeEmptyParams(request.Params); decodeErr != nil {
			err = decodeErr
		} else {
			result, err = supervisor.Wait(ctx, request.Target, request.IfGeneration)
		}
	case guestproto.OperationShutdown:
		if decodeErr := decodeEmptyParams(request.Params); decodeErr != nil {
			err = decodeErr
		} else {
			err = supervisor.Shutdown(ctx)
			if err == nil {
				result = map[string]any{}
			}
		}
	default:
		err = &codedError{code: guestproto.CodeInvalidRequest, message: "unknown operation"}
	}
	if err != nil {
		var protocolErr *codedError
		if !errors.As(err, &protocolErr) {
			protocolErr = &codedError{code: guestproto.CodeInternal, message: "guest operation failed", cause: err}
		}
		return supervisor.errorResponse(request.RequestID, protocolErr.code, protocolErr.message, protocolErr.generation)
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return supervisor.errorResponse(request.RequestID, guestproto.CodeInternal, "encode operation result", nil)
	}
	return guestproto.Response{Protocol: guestproto.ProtocolV1, RequestID: request.RequestID, OK: true, Result: encoded, Server: supervisor.ServerIdentity()}
}

func (supervisor *Supervisor) Start(ctx context.Context, expected uint64, params guestproto.StartParams) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := params.Validate(); err != nil {
		return supervisor.currentGeneration(), &codedError{code: guestproto.CodeInvalidRequest, message: "invalid process graph", cause: err}
	}
	if err := validateHealthTargets(params); err != nil {
		return supervisor.currentGeneration(), &codedError{code: guestproto.CodeInvalidRequest, message: "invalid readiness target", cause: err}
	}
	supervisor.mu.Lock()
	current := supervisor.generation
	if expected != current {
		supervisor.mu.Unlock()
		return current, &codedError{code: guestproto.CodeGenerationConflict, message: "start generation does not match", generation: &current}
	}
	if supervisor.shuttingDown {
		supervisor.mu.Unlock()
		return current, &codedError{code: guestproto.CodeShuttingDown, message: "guest is shutting down"}
	}
	for _, existing := range supervisor.processes {
		if !channelClosed(existing.doneCh) {
			supervisor.mu.Unlock()
			return current, &codedError{code: guestproto.CodeWrongState, message: "cannot replace an active process graph"}
		}
	}
	supervisor.mu.Unlock()

	for index := range params.Setup {
		if err := runOne(ctx, params.Setup[index], supervisor.childUID, supervisor.childGID); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return current, deadlineError(err)
			}
			return current, &codedError{code: guestproto.CodeStartFailed, message: fmt.Sprintf("setup command %d failed", index+1), cause: redactExecError(err)}
		}
	}
	reapOrphans()

	supervisor.mu.Lock()
	if supervisor.generation != expected {
		current = supervisor.generation
		supervisor.mu.Unlock()
		return current, &codedError{code: guestproto.CodeGenerationConflict, message: "start generation changed", generation: &current}
	}
	generation := expected + 1
	processes := make(map[string]*process, len(params.Processes))
	order := make([]string, 0, len(params.Processes))
	retainedLogLimit := effectiveRetainedLogLimit(params.LogLimitBytes, len(params.Processes))
	for index := range params.Processes {
		spec := params.Processes[index]
		processes[spec.ID] = &process{
			spec:        spec,
			generation:  generation,
			state:       guestproto.StateConfigured,
			log:         newProcessLog(spec.ID, retainedLogLimit, supervisor.output),
			readyCh:     make(chan struct{}),
			doneCh:      make(chan struct{}),
			controlDone: make(chan struct{}),
		}
		order = append(order, spec.ID)
	}
	supervisor.processes = processes
	supervisor.order = order
	supervisor.generation = generation
	supervisor.failed = false
	for _, id := range order {
		process := processes[id]
		process.state = guestproto.StateWaitingDependencies
		go supervisor.runProcess(process)
	}
	supervisor.mu.Unlock()
	return generation, nil
}

func (supervisor *Supervisor) runProcess(process *process) {
	defer close(process.controlDone)
	for _, dependencyID := range process.spec.DependsOn {
		supervisor.mu.Lock()
		dependency := supervisor.processes[dependencyID]
		supervisor.mu.Unlock()
		select {
		case <-dependency.readyCh:
		case <-dependency.doneCh:
			supervisor.failWithoutProcess(process, "dependency failed before readiness")
			return
		case <-supervisor.done:
			supervisor.failWithoutProcess(process, "guest shut down before process start")
			return
		}
	}

	supervisor.mu.Lock()
	if supervisor.shuttingDown {
		supervisor.mu.Unlock()
		supervisor.failWithoutProcess(process, "guest shut down before process start")
		return
	}
	process.state = guestproto.StateStarting
	command := commandFor(process.spec.Command, process.log, supervisor.childUID, supervisor.childGID)
	process.cmd = command
	var startErr error
	if process.spec.Terminal {
		attributes := *command.SysProcAttr
		attributes.Setpgid = false
		startErr = startDirectChild(func() error {
			var err error
			process.terminal, err = pty.StartWithAttrs(command, &pty.Winsize{Rows: 24, Cols: 80}, &attributes)
			return err
		}, func() int { return command.Process.Pid })
		if startErr == nil {
			terminal := process.terminal
			process.terminalDone = make(chan struct{})
			terminalDone := process.terminalDone
			go func() {
				_, _ = io.Copy(process.log, terminal)
				close(terminalDone)
			}()
		}
	} else {
		startErr = startDirectChild(command.Start, func() int { return command.Process.Pid })
	}
	if startErr != nil {
		process.state = guestproto.StateFailed
		process.failure = "process start failed"
		if process.spec.Required {
			supervisor.failed = true
		}
		process.doneOnce.Do(func() { close(process.doneCh) })
		supervisor.mu.Unlock()
		return
	}
	process.state = guestproto.StateRunning
	supervisor.mu.Unlock()
	if process.spec.Health == nil {
		supervisor.markReady(process)
		go supervisor.waitProcess(process, command)
		return
	}
	go supervisor.waitProcess(process, command)
	if err := waitForHealth(process.doneCh, process.spec.Health, supervisor.childUID, supervisor.childGID); err != nil {
		supervisor.mu.Lock()
		if !channelClosed(process.doneCh) {
			process.state = guestproto.StateFailed
			process.failure = "readiness check failed"
			if process.spec.Required && !supervisor.shuttingDown {
				supervisor.failed = true
			}
			pid := command.Process.Pid
			supervisor.mu.Unlock()
			_ = signalGroup(pid, syscall.SIGKILL)
			return
		}
		supervisor.mu.Unlock()
		return
	}
	supervisor.markReady(process)
	supervisor.monitorHealth(process)
}

func (supervisor *Supervisor) waitProcess(process *process, command *exec.Cmd) {
	err := command.Wait()
	unregisterDirectChild(command.Process.Pid)
	_ = signalGroup(command.Process.Pid, syscall.SIGKILL)
	terminateAdoptedChildren(supervisor.directChildPIDs)
	supervisor.closeProcessTerminal(process)
	process.log.Flush()
	exit := exitStatus(command, err)
	supervisor.mu.Lock()
	process.exit = exit
	wasStopping := process.state == guestproto.StateStopping || supervisor.shuttingDown
	wasReady := process.ready
	if process.state != guestproto.StateFailed {
		process.state = guestproto.StateExited
	}
	process.ready = false
	if !wasStopping {
		if !wasReady && process.failure == "" {
			process.failure = "process exited before readiness"
		}
		if process.spec.Required {
			supervisor.failed = true
		}
	}
	process.doneOnce.Do(func() { close(process.doneCh) })
	supervisor.mu.Unlock()
}
func (supervisor *Supervisor) closeProcessTerminal(process *process) {
	supervisor.mu.Lock()
	terminal := process.terminal
	done := process.terminalDone
	process.terminal = nil
	supervisor.mu.Unlock()
	if terminal == nil {
		return
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = terminal.Close()
	if done != nil {
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (supervisor *Supervisor) directChildPIDs() map[int]struct{} {
	return managedDirectChildPIDs()
}

func managedDirectChildPIDs() map[int]struct{} {
	pids := make(map[int]struct{})
	managedDirectChildren.Range(func(key, _ any) bool {
		pids[key.(int)] = struct{}{}
		return true
	})
	return pids
}

var (
	managedDirectChildren   sync.Map
	managedDirectChildrenMu sync.Mutex
)

func startDirectChild(start func() error, pid func() int) error {
	managedDirectChildrenMu.Lock()
	defer managedDirectChildrenMu.Unlock()
	if err := startWithoutPrivilegeGains(start); err != nil {
		return err
	}
	managedDirectChildren.Store(pid(), struct{}{})
	return nil
}

func unregisterDirectChild(pid int) {
	managedDirectChildrenMu.Lock()
	managedDirectChildren.Delete(pid)
	managedDirectChildrenMu.Unlock()
}

func (supervisor *Supervisor) monitorHealth(process *process) {
	interval := time.Duration(process.spec.Health.IntervalMS) * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-process.doneCh:
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(process.spec.Health.TimeoutMS)*time.Millisecond)
		err := probe(ctx, process.spec.Health, supervisor.childUID, supervisor.childGID)
		cancel()
		supervisor.mu.Lock()
		if channelClosed(process.doneCh) {
			supervisor.mu.Unlock()
			return
		}
		if err == nil {
			failures = 0
			if process.state == guestproto.StateUnhealthy {
				process.state = guestproto.StateReady
				process.ready = true
				process.failure = ""
				supervisor.refreshFailedLocked()
			}
		} else {
			failures++
			if failures >= process.spec.Health.Retries {
				process.state = guestproto.StateUnhealthy
				process.ready = false
				process.failure = "health check failed"
				if process.spec.Required {
					supervisor.failed = true
				}
			}
		}
		supervisor.mu.Unlock()
		timer.Reset(interval)
	}
}

func (supervisor *Supervisor) refreshFailedLocked() {
	supervisor.failed = false
	for _, process := range supervisor.processes {
		if !process.spec.Required {
			continue
		}
		switch process.state {
		case guestproto.StateFailed, guestproto.StateUnhealthy, guestproto.StateExited:
			supervisor.failed = true
			return
		}
	}
}

func (supervisor *Supervisor) markReady(process *process) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if channelClosed(process.doneCh) || process.state == guestproto.StateFailed || supervisor.shuttingDown {
		return
	}
	process.ready = true
	process.state = guestproto.StateReady
	process.readyOnce.Do(func() { close(process.readyCh) })
}

func (supervisor *Supervisor) failWithoutProcess(process *process, message string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if channelClosed(process.doneCh) {
		return
	}
	process.state = guestproto.StateFailed
	process.failure = message
	if process.spec.Required && !supervisor.shuttingDown {
		supervisor.failed = true
	}
	process.doneOnce.Do(func() { close(process.doneCh) })
}

func (supervisor *Supervisor) Signal(target string, expected uint64, name string) error {
	signal, found := allowedSignal(name)
	if !found {
		return &codedError{code: guestproto.CodeInvalidRequest, message: "signal is not allowed"}
	}
	supervisor.mu.Lock()
	if err := supervisor.checkGenerationLocked(&expected); err != nil {
		supervisor.mu.Unlock()
		return err
	}
	process, found := supervisor.processes[target]
	if !found {
		supervisor.mu.Unlock()
		return &codedError{code: guestproto.CodeNotFound, message: "configured process was not found"}
	}
	if process.cmd == nil || process.cmd.Process == nil || channelClosed(process.doneCh) {
		supervisor.mu.Unlock()
		return &codedError{code: guestproto.CodeWrongState, message: "process is not running"}
	}
	pid := process.cmd.Process.Pid
	if signal == syscall.SIGTERM || signal == syscall.SIGINT || signal == syscall.SIGKILL {
		process.state = guestproto.StateStopping
	}
	supervisor.mu.Unlock()
	if err := signalGroup(pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return &codedError{code: guestproto.CodeSignalFailed, message: "signal process group", cause: err}
	}
	return nil
}

func (supervisor *Supervisor) Resize(target string, expected uint64, columns, rows uint16) error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if err := supervisor.checkGenerationLocked(&expected); err != nil {
		return err
	}
	process, found := supervisor.processes[target]
	if !found {
		return &codedError{code: guestproto.CodeNotFound, message: "configured process was not found"}
	}
	if process.terminal == nil || channelClosed(process.doneCh) {
		return &codedError{code: guestproto.CodeResizeFailed, message: "process is not attached to a resizable terminal"}
	}
	if err := pty.Setsize(process.terminal, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		return &codedError{code: guestproto.CodeResizeFailed, message: "resize process terminal", cause: err}
	}
	return nil
}

func (supervisor *Supervisor) Wait(ctx context.Context, target string, expected *uint64) (*guestproto.ExitStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	supervisor.mu.Lock()
	if err := supervisor.checkGenerationLocked(expected); err != nil {
		supervisor.mu.Unlock()
		return nil, err
	}
	process, found := supervisor.processes[target]
	if !found {
		supervisor.mu.Unlock()
		return nil, &codedError{code: guestproto.CodeNotFound, message: "configured process was not found"}
	}
	done := process.doneCh
	supervisor.mu.Unlock()
	select {
	case <-done:
		status, err := supervisor.processStatus(target)
		if err != nil {
			return nil, err
		}
		if status.Exit == nil {
			return nil, &codedError{code: guestproto.CodeStartFailed, message: "process did not start"}
		}
		return status.Exit, nil
	case <-ctx.Done():
		return nil, deadlineError(ctx.Err())
	}
}

func (supervisor *Supervisor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	supervisor.mu.Lock()
	if channelClosed(supervisor.done) {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.shuttingDown = true
	var running []*process
	allProcesses := make([]*process, 0, len(supervisor.order))
	for _, id := range supervisor.order {
		process := supervisor.processes[id]
		allProcesses = append(allProcesses, process)
		if process.cmd != nil && process.cmd.Process != nil && !channelClosed(process.doneCh) {
			process.state = guestproto.StateStopping
			running = append(running, process)
		} else if !channelClosed(process.doneCh) {
			process.state = guestproto.StateFailed
			process.failure = "guest shut down before process start"
			process.doneOnce.Do(func() { close(process.doneCh) })
		}
	}
	supervisor.mu.Unlock()
	for _, process := range running {
		_ = signalGroup(process.cmd.Process.Pid, syscall.SIGTERM)
	}
	allDone := waitAll(allProcesses)
	if len(running) == 0 {
		select {
		case <-allDone:
			supervisor.finishShutdown()
			return nil
		case <-ctx.Done():
			go func() { <-allDone; supervisor.finishShutdown() }()
			return deadlineError(ctx.Err())
		}
	}
	grace := time.NewTimer(supervisor.shutdownGrace)
	defer grace.Stop()
	select {
	case <-allDone:
		supervisor.finishShutdown()
		return nil
	case <-grace.C:
	case <-ctx.Done():
	}
	for _, process := range running {
		if !channelClosed(process.doneCh) {
			_ = signalGroup(process.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	select {
	case <-allDone:
		supervisor.finishShutdown()
		return nil
	case <-ctx.Done():
		go func() { <-allDone; supervisor.finishShutdown() }()
		return deadlineError(ctx.Err())
	}
}
func (supervisor *Supervisor) finishShutdown() {
	reapOrphans()
	supervisor.output.Close()
	supervisor.doneOnce.Do(func() { close(supervisor.done) })
}

func (supervisor *Supervisor) Status() guestproto.StatusResult {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	result := guestproto.StatusResult{Generation: supervisor.generation, Failed: supervisor.failed, Processes: make([]guestproto.ProcessStatus, 0, len(supervisor.order))}
	logLimit := 0
	if len(supervisor.order) != 0 {
		logLimit = (guestproto.MaxFrameSize / 2) / len(supervisor.order)
	}
	for _, id := range supervisor.order {
		result.Processes = append(result.Processes, snapshotProcessWithLimit(supervisor.processes[id], logLimit))
	}
	return result
}

func (supervisor *Supervisor) processStatus(target string) (guestproto.ProcessStatus, error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	process, found := supervisor.processes[target]
	if !found {
		return guestproto.ProcessStatus{}, &codedError{code: guestproto.CodeNotFound, message: "configured process was not found"}
	}
	return snapshotProcess(process), nil
}

func boundedStatusJSON(status guestproto.StatusResult) (json.RawMessage, error) {
	const maxStatusResultBytes = guestproto.MaxFrameSize - 8192
	for {
		encoded, err := json.Marshal(status)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxStatusResultBytes {
			return encoded, nil
		}
		largest := -1
		for index := range status.Processes {
			if largest == -1 || len(status.Processes[index].Log) > len(status.Processes[largest].Log) {
				largest = index
			}
		}
		if largest == -1 || len(status.Processes[largest].Log) == 0 {

			return nil, errors.New("status metadata exceeds protocol frame")
		}
		log := status.Processes[largest].Log
		remove := len(log) / 2
		if remove < 1 {
			remove = 1
		}
		status.Processes[largest].Log = log[remove:]
		status.Processes[largest].LogDropped += uint64(remove)
	}
}

func snapshotProcess(process *process) guestproto.ProcessStatus {
	log, dropped := process.log.Snapshot()
	return guestproto.ProcessStatus{ID: process.spec.ID, Generation: process.generation, State: process.state, Ready: process.ready, Required: process.spec.Required, Exit: cloneExit(process.exit), Failure: process.failure, Log: log, LogDropped: dropped}
}

func snapshotProcessWithLimit(process *process, limit int) guestproto.ProcessStatus {
	log, dropped := process.log.SnapshotLimit(limit)
	return guestproto.ProcessStatus{ID: process.spec.ID, Generation: process.generation, State: process.state, Ready: process.ready, Required: process.spec.Required, Exit: cloneExit(process.exit), Failure: process.failure, Log: log, LogDropped: dropped}
}

func (supervisor *Supervisor) checkGenerationLocked(expected *uint64) error {
	if supervisor.generation == 0 {
		return &codedError{code: guestproto.CodeWrongState, message: "guest has not been started"}
	}
	if expected != nil && *expected != supervisor.generation {
		current := supervisor.generation
		return &codedError{code: guestproto.CodeGenerationConflict, message: "start generation does not match", generation: &current}
	}
	return nil
}

func (supervisor *Supervisor) currentGeneration() uint64 {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.generation
}

func (supervisor *Supervisor) errorResponse(requestID string, code guestproto.ErrorCode, message string, generation *uint64) guestproto.Response {
	if !guestproto.ValidUUID(requestID) {
		requestID = "00000000-0000-0000-0000-000000000000"
	}
	return guestproto.Response{Protocol: guestproto.ProtocolV1, RequestID: requestID, OK: false, Error: &guestproto.Error{Code: code, Message: message, Generation: generation}, Server: supervisor.ServerIdentity()}
}

func (supervisor *Supervisor) storeIdempotency(key string, fingerprint [sha256.Size]byte, response guestproto.Response) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if len(supervisor.idempotencyOrder) == maxIdempotencyEntries {
		oldest := supervisor.idempotencyOrder[0]
		delete(supervisor.idempotency, oldest)
		copy(supervisor.idempotencyOrder, supervisor.idempotencyOrder[1:])
		supervisor.idempotencyOrder = supervisor.idempotencyOrder[:len(supervisor.idempotencyOrder)-1]
	}
	supervisor.idempotency[key] = idempotencyEntry{fingerprint: fingerprint, response: cloneResponse(response)}
	supervisor.idempotencyOrder = append(supervisor.idempotencyOrder, key)
}

func requestFingerprint(request guestproto.Request) [sha256.Size]byte {
	encoded, _ := json.Marshal(request)
	return sha256.Sum256(encoded)
}

func cloneResponse(response guestproto.Response) guestproto.Response {
	copyResponse := response
	copyResponse.Result = append(json.RawMessage(nil), response.Result...)
	if response.Error != nil {
		copyError := *response.Error
		copyError.Details = append(json.RawMessage(nil), response.Error.Details...)
		copyResponse.Error = &copyError
	}
	return copyResponse
}

func cloneExit(exit *guestproto.ExitStatus) *guestproto.ExitStatus {
	if exit == nil {
		return nil
	}
	copyExit := *exit
	if exit.Code != nil {
		code := *exit.Code
		copyExit.Code = &code
	}
	return &copyExit
}

func isMutation(operation guestproto.Operation) bool {
	switch operation {
	case guestproto.OperationStart, guestproto.OperationSignal, guestproto.OperationResize, guestproto.OperationShutdown:
		return true
	default:
		return false
	}
}

func decodeEmptyParams(raw json.RawMessage) error {
	var params struct{}
	if err := guestproto.DecodeParams(raw, &params); err != nil {
		return &codedError{code: guestproto.CodeInvalidRequest, message: "operation parameters must be empty", cause: err}
	}
	return nil
}

func commandFor(spec guestproto.CommandSpec, output io.Writer, uid, gid uint32) *exec.Cmd {
	command := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	command.Dir = spec.Cwd
	command.Env = childEnvironment(spec.Env)
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = processGroupAttributes(uid, gid)
	command.WaitDelay = 100 * time.Millisecond
	return command
}
func childEnvironment(overrides []string) []string {
	environment := make([]string, 0, 10+len(overrides))
	indexes := make(map[string]int, 10+len(overrides))
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found || !allowedBaselineEnvironment(name) {
			continue
		}
		if _, duplicate := indexes[name]; duplicate {
			continue
		}
		indexes[name] = len(environment)
		environment = append(environment, entry)
	}
	for _, entry := range overrides {
		name, _, _ := strings.Cut(entry, "=")
		if index, found := indexes[name]; found {
			environment[index] = entry
			continue
		}

		indexes[name] = len(environment)
		environment = append(environment, entry)
	}
	return environment
}
func effectiveRetainedLogLimit(requested, processes int) int {
	if processes <= 0 {
		return requested
	}
	const maxAggregateRetainedLogs = 32 << 20
	if share := maxAggregateRetainedLogs / processes; requested > share {
		return share
	}
	return requested
}

func allowedBaselineEnvironment(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TMPDIR",
		"DSX_PROJECT_ID", "DSX_SANDBOX", "DSX_RUN_ID":
		return true
	default:
		return false
	}
}

func runOne(ctx context.Context, spec guestproto.CommandSpec, uid, gid uint32) error {
	command := commandFor(spec, nil, uid, gid)
	if err := startDirectChild(command.Start, func() int { return command.Process.Pid }); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		unregisterDirectChild(command.Process.Pid)
		_ = signalGroup(command.Process.Pid, syscall.SIGKILL)
		terminateAdoptedChildren(managedDirectChildPIDs)
		return err
	case <-ctx.Done():
		_ = signalGroup(command.Process.Pid, syscall.SIGKILL)
		<-done
		unregisterDirectChild(command.Process.Pid)
		terminateAdoptedChildren(managedDirectChildPIDs)
		return ctx.Err()
	}
}

func waitForHealth(processDone <-chan struct{}, spec *guestproto.HealthSpec, uid, gid uint32) error {
	interval := time.Duration(spec.IntervalMS) * time.Millisecond
	var last error
	for attempt := 0; attempt < spec.Retries; attempt++ {
		select {
		case <-processDone:
			return errors.New("process exited before readiness")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(spec.TimeoutMS)*time.Millisecond)
		probeFinished := make(chan struct{})
		go func() {
			select {
			case <-processDone:
				cancel()
			case <-probeFinished:
			}
		}()
		last = probe(ctx, spec, uid, gid)
		close(probeFinished)
		cancel()
		if last == nil {
			return nil
		}
		if attempt+1 == spec.Retries {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-processDone:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.New("process exited before readiness")
		}
	}
	return last
}

func probe(ctx context.Context, spec *guestproto.HealthSpec, uid, gid uint32) error {
	switch spec.Kind {
	case "tcp":
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", spec.Target)
		if err != nil {
			return err
		}
		return connection.Close()
	case "http":
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.Target, nil)
		if err != nil {
			return err
		}
		client := http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{}).DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return fmt.Errorf("HTTP readiness status %d", response.StatusCode)
		}
		return nil
	case "command":
		return runOne(ctx, *spec.Command, uid, gid)
	default:
		return errors.New("unsupported readiness kind")
	}
}

func validateHealthTargets(params guestproto.StartParams) error {
	for _, process := range params.Processes {
		if process.Health == nil {
			continue
		}
		switch process.Health.Kind {
		case "tcp":
			address, err := netip.ParseAddrPort(process.Health.Target)
			if err != nil || !address.Addr().IsLoopback() {
				return fmt.Errorf("process %q TCP readiness must target loopback", process.ID)
			}
		case "http":
			parsed, err := url.Parse(process.Health.Target)
			if err != nil || !loopbackHost(parsed.Hostname()) {
				return fmt.Errorf("process %q HTTP readiness must target loopback", process.ID)
			}
		}
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func exitStatus(command *exec.Cmd, waitErr error) *guestproto.ExitStatus {
	if command.ProcessState == nil {
		return nil
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		code := command.ProcessState.ExitCode()
		return &guestproto.ExitStatus{Code: &code}
	}
	if status.Signaled() {
		return &guestproto.ExitStatus{Signal: signalName(status.Signal())}
	}
	code := status.ExitStatus()
	return &guestproto.ExitStatus{Code: &code}
}

func allowedSignal(name string) (syscall.Signal, bool) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, true
	case "INT":
		return syscall.SIGINT, true
	case "KILL":
		return syscall.SIGKILL, true
	case "HUP":
		return syscall.SIGHUP, true
	default:
		return 0, false
	}
}

func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGQUIT:
		return "QUIT"
	default:
		return strings.TrimPrefix(strings.ToUpper(signal.String()), "SIG")
	}
}

func signalGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(-pid, signal)
}

func processGroupAttributes(uid, gid uint32) *syscall.SysProcAttr {
	credential := &syscall.Credential{Uid: uid, Gid: gid}
	if os.Geteuid() == 0 {
		credential.Groups = []uint32{gid}
	} else {
		credential.NoSetGroups = true
	}
	return &syscall.SysProcAttr{Setpgid: true, Credential: credential}
}

func waitAll(processes []*process) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for _, process := range processes {
			<-process.doneCh
			<-process.controlDone
		}
		close(done)
	}()
	return done
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func deadlineError(err error) error {
	return &codedError{code: guestproto.CodeDeadlineExceeded, message: "operation deadline exceeded", cause: err}
}

func redactExecError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errors.New("command exited unsuccessfully")
	}
	return errors.New("command could not be executed")
}

func newUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
