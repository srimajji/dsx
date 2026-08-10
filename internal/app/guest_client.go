package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	DefaultGuestHelperDirectory = "/usr/local/libexec/dsx"
	DefaultGuestHelperPath      = DefaultGuestHelperDirectory + "/dsx-guest"
	DefaultGuestSocketPath      = guestproto.DefaultSocketPath

	defaultGuestRequestTimeout = 5 * time.Second
	defaultGuestAttempts       = 3
	defaultGuestRetryDelay     = 50 * time.Millisecond
	maxGuestAttempts           = 5
	maxGuestStderrBytes        = 4096
)

type GuestClientDependencies struct {
	Adapter        runtime.Adapter
	HelperPath     string
	SocketPath     string
	RequestTimeout time.Duration
	RetryDelay     time.Duration
	MaxAttempts    int
	LogLimitBytes  int
	UUID           func() (string, error)
	Now            func() time.Time
	User           string
}

type GuestClient struct {
	adapter        runtime.Adapter
	helperPath     string
	socketPath     string
	requestTimeout time.Duration
	retryDelay     time.Duration
	maxAttempts    int
	logLimitBytes  int
	uuid           func() (string, error)
	now            func() time.Time
	user           string

	serverMu        sync.Mutex
	serverInstances map[runtime.ResourceID]string
}

func NewGuestClient(adapter runtime.Adapter) *GuestClient {
	return NewGuestClientWithDependencies(GuestClientDependencies{Adapter: adapter})
}
func NewGuestClientWithDependencies(dependencies GuestClientDependencies) *GuestClient {
	helperPath := dependencies.HelperPath
	if helperPath == "" {
		helperPath = DefaultGuestHelperPath
	}
	socketPath := dependencies.SocketPath
	if socketPath == "" {
		socketPath = DefaultGuestSocketPath
	}
	requestTimeout := dependencies.RequestTimeout
	if requestTimeout <= 0 || requestTimeout > time.Duration(guestproto.MaxDeadlineMS)*time.Millisecond {
		requestTimeout = defaultGuestRequestTimeout
	}
	retryDelay := dependencies.RetryDelay
	if retryDelay <= 0 || retryDelay > time.Second {
		retryDelay = defaultGuestRetryDelay
	}
	maxAttempts := dependencies.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultGuestAttempts
	}
	if maxAttempts > maxGuestAttempts {
		maxAttempts = maxGuestAttempts
	}
	logLimitBytes := dependencies.LogLimitBytes
	if logLimitBytes == 0 {
		logLimitBytes = guestproto.MaxLogBytes
	}
	uuid := dependencies.UUID
	if uuid == nil {
		uuid = newGuestUUID
	}
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	user := dependencies.User
	if user == "" {
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return &GuestClient{
		adapter:         dependencies.Adapter,
		helperPath:      helperPath,
		socketPath:      socketPath,
		requestTimeout:  requestTimeout,
		retryDelay:      retryDelay,
		maxAttempts:     maxAttempts,
		logLimitBytes:   logLimitBytes,
		uuid:            uuid,
		now:             now,
		user:            user,
		serverInstances: make(map[runtime.ResourceID]string),
	}
}

// Reconcile accepts the control-server instance created by a workspace start.
// It is intentionally separate from ordinary reads and mutations so an
// unexpected replacement in a continuously running workspace remains an error.
func (client *GuestClient) Reconcile(ctx context.Context, workspace runtime.ResourceSnapshot) error {
	_, err := client.probe(ctx, workspace)
	if model.ErrorCodeOf(err) == model.CodeConflict {
		return nil
	}
	return err
}

func (client *GuestClient) Start(ctx context.Context, workspace runtime.ResourceSnapshot, approved plan.ExecutionPlan, ifGeneration uint64) (guestproto.StartResult, error) {
	var result guestproto.StartResult
	params, err := client.startParams(approved)
	if err != nil {
		return result, err
	}
	if err := client.mutate(ctx, workspace, guestproto.OperationStart, "", &ifGeneration, params, &result); err != nil {
		return guestproto.StartResult{}, err
	}
	return result, nil
}

func (client *GuestClient) Status(ctx context.Context, workspace runtime.ResourceSnapshot) (guestproto.StatusResult, error) {
	var result guestproto.StatusResult
	if err := client.query(ctx, workspace, guestproto.OperationStatus, "", nil, struct{}{}, &result); err != nil {
		return guestproto.StatusResult{}, err
	}
	if err := client.validateStatus(result); err != nil {
		return guestproto.StatusResult{}, err
	}
	return result, nil
}

func (client *GuestClient) Signal(ctx context.Context, workspace runtime.ResourceSnapshot, target string, ifGeneration uint64, signal string) error {
	if !allowedGuestSignal(signal) {
		return model.NewError(model.CodeInvalidInput, "guest signal is not allowed", nil)
	}
	return client.mutate(ctx, workspace, guestproto.OperationSignal, target, &ifGeneration, guestproto.SignalParams{Signal: signal}, &emptyGuestResult{})
}

func (client *GuestClient) Resize(ctx context.Context, workspace runtime.ResourceSnapshot, target string, ifGeneration uint64, columns, rows uint16) error {
	if columns == 0 || rows == 0 {
		return model.NewError(model.CodeInvalidInput, "guest terminal size must be non-zero", nil)
	}
	return client.mutate(ctx, workspace, guestproto.OperationResize, target, &ifGeneration, guestproto.ResizeParams{Columns: columns, Rows: rows}, &emptyGuestResult{})
}

func (client *GuestClient) Wait(ctx context.Context, workspace runtime.ResourceSnapshot, target string, ifGeneration uint64) (guestproto.ExitStatus, error) {
	var result guestproto.ExitStatus
	if err := client.query(ctx, workspace, guestproto.OperationWait, target, &ifGeneration, struct{}{}, &result); err != nil {
		return guestproto.ExitStatus{}, err
	}
	if err := validateGuestExit(result); err != nil {
		return guestproto.ExitStatus{}, err
	}
	return result, nil
}

func (client *GuestClient) Shutdown(ctx context.Context, workspace runtime.ResourceSnapshot) error {
	return client.mutate(ctx, workspace, guestproto.OperationShutdown, "", nil, struct{}{}, &emptyGuestResult{})
}

type emptyGuestResult struct{}

func (client *GuestClient) startParams(approved plan.ExecutionPlan) (guestproto.StartParams, error) {
	params := guestproto.StartParams{
		Setup:         make([]guestproto.CommandSpec, len(approved.Setup)),
		Processes:     make([]guestproto.ProcessSpec, len(approved.Processes)),
		LogLimitBytes: client.logLimitBytes,
	}
	for index := range approved.Setup {
		command, err := convertGuestCommand(approved.Setup[index])
		if err != nil {
			return guestproto.StartParams{}, err
		}
		params.Setup[index] = command
	}
	for index := range approved.Processes {
		resolved := approved.Processes[index]
		command, err := convertGuestCommand(resolved.Command)
		if err != nil {
			return guestproto.StartParams{}, err
		}
		process := guestproto.ProcessSpec{
			ID:        resolved.Name,
			Command:   command,
			DependsOn: append([]string(nil), resolved.DependsOn...),
			Required:  resolved.Required,
			Terminal:  resolved.Terminal,
		}
		if resolved.Health != nil {
			health := &guestproto.HealthSpec{
				Kind:       resolved.Health.Kind,
				Target:     resolved.Health.Target,
				IntervalMS: resolved.Health.IntervalMS,
				TimeoutMS:  resolved.Health.TimeoutMS,
				Retries:    resolved.Health.Retries,
			}
			if resolved.Health.Command != nil {
				healthCommand, convertErr := convertGuestCommand(*resolved.Health.Command)
				if convertErr != nil {
					return guestproto.StartParams{}, convertErr
				}
				health.Command = &healthCommand
			}
			process.Health = health
		}
		params.Processes[index] = process
	}
	if err := params.Validate(); err != nil {
		return guestproto.StartParams{}, model.NewError(model.CodeInvalidInput, "approved guest process graph is invalid", nil)
	}
	return params, nil
}

func convertGuestCommand(command plan.ResolvedCommand) (guestproto.CommandSpec, error) {
	var argv []string
	switch {
	case len(command.Argv) != 0 && command.Shell == "" && command.ShellPath == "":
		argv = append([]string(nil), command.Argv...)
	case len(command.Argv) == 0 && command.Shell != "" && command.ShellPath != "":
		argv = []string{command.ShellPath, "-lc", command.Shell}
	default:
		return guestproto.CommandSpec{}, model.NewError(model.CodeInvalidInput, "approved guest command is invalid", nil)
	}
	environment := make([]string, len(command.Env))
	for index, grant := range command.Env {
		environment[index] = grant.Name + "=" + grant.Value
	}
	cwd := command.Cwd
	if cwd == "" {
		cwd = string(workspaceGuestRoot)
	}
	converted := guestproto.CommandSpec{Argv: argv, Cwd: cwd, Env: environment}
	if err := converted.Validate(); err != nil {
		return guestproto.CommandSpec{}, model.NewError(model.CodeInvalidInput, "approved guest command is invalid", nil)
	}
	return converted, nil
}

func (client *GuestClient) query(ctx context.Context, workspace runtime.ResourceSnapshot, operation guestproto.Operation, target string, generation *uint64, params, result any) error {
	callContext, cancel, err := client.callContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	request, encoded, err := client.request(callContext, operation, target, generation, "", params)
	if err != nil {
		return err
	}
	response, err := client.retryRead(callContext, workspace, request.RequestID, encoded)
	if err != nil {
		return err
	}
	if changed := client.observeInstance(workspace.ID, response.Server.InstanceID); changed {
		return model.NewError(model.CodeUnavailable, "guest helper instance changed; retry after status reconciliation", nil)
	}
	return decodeGuestResponse(response, result)
}

func (client *GuestClient) mutate(ctx context.Context, workspace runtime.ResourceSnapshot, operation guestproto.Operation, target string, generation *uint64, params, result any) error {
	callContext, cancel, err := client.callContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := client.validateWorkspace(workspace); err != nil {
		return err
	}
	key, err := client.nextUUID()
	if err != nil {
		return err
	}
	request, encoded, err := client.request(callContext, operation, target, generation, key, params)
	if err != nil {
		return err
	}
	if _, err := client.probe(callContext, workspace); err != nil {
		return err
	}
	for attempt := range client.maxAttempts {
		response, transportErr := client.execOnce(callContext, workspace, request.RequestID, encoded)
		if transportErr == nil {
			if changed := client.observeInstance(workspace.ID, response.Server.InstanceID); changed {
				return model.NewError(model.CodeConflict, "guest helper instance changed during mutation; reconcile status before retrying", nil)
			}
			return decodeGuestResponse(response, result)
		}
		if err := contextGuestError(callContext); err != nil {
			return err
		}
		if !errors.Is(transportErr, errGuestTransport) {
			return guestControlProtocolError(transportErr)
		}
		if attempt+1 == client.maxAttempts {
			return guestTransportError()
		}
		if err := client.waitRetry(callContext); err != nil {
			return err
		}
		if _, err := client.probe(callContext, workspace); err != nil {
			return err
		}
	}
	return guestTransportError()
}

func (client *GuestClient) probe(ctx context.Context, workspace runtime.ResourceSnapshot) (guestproto.Server, error) {
	request, encoded, err := client.request(ctx, guestproto.OperationPing, "", nil, "", struct{}{})
	if err != nil {
		return guestproto.Server{}, err
	}
	response, err := client.retryRead(ctx, workspace, request.RequestID, encoded)
	if err != nil {
		return guestproto.Server{}, err
	}
	if err := decodeGuestResponse(response, &emptyGuestResult{}); err != nil {
		return guestproto.Server{}, err
	}
	if changed := client.observeInstance(workspace.ID, response.Server.InstanceID); changed {
		return response.Server, model.NewError(model.CodeConflict, "guest helper instance changed; reconcile status before mutating", nil)
	}
	return response.Server, nil
}

func (client *GuestClient) retryRead(ctx context.Context, workspace runtime.ResourceSnapshot, requestID string, encoded []byte) (guestproto.Response, error) {
	if err := client.validateWorkspace(workspace); err != nil {
		return guestproto.Response{}, err
	}
	for attempt := range client.maxAttempts {
		response, err := client.execOnce(ctx, workspace, requestID, encoded)
		if err == nil {
			return response, nil
		}
		if contextErr := contextGuestError(ctx); contextErr != nil {
			return guestproto.Response{}, contextErr
		}
		if !errors.Is(err, errGuestTransport) {
			return guestproto.Response{}, guestControlProtocolError(err)
		}
		if attempt+1 == client.maxAttempts {
			return guestproto.Response{}, guestTransportError()
		}
		if err := client.waitRetry(ctx); err != nil {
			return guestproto.Response{}, err
		}
	}
	return guestproto.Response{}, guestTransportError()
}

func (client *GuestClient) waitRetry(ctx context.Context) error {
	timer := time.NewTimer(client.retryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return contextGuestError(ctx)
	case <-timer.C:
		return nil
	}
}

func (client *GuestClient) execOnce(ctx context.Context, workspace runtime.ResourceSnapshot, requestID string, encoded []byte) (guestproto.Response, error) {
	stdout := newGuestCapture(guestproto.MaxFrameSize + 1)
	stderr := newGuestCapture(maxGuestStderrBytes)
	exit, err := client.adapter.Exec(ctx, workspace, runtime.ExecSpec{
		Argv: []string{client.helperPath, "ctl", "--socket", client.socketPath},
		User: client.user,
	}, runtime.ExecIO{
		Stdin:  bytes.NewReader(encoded),
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil || exit.Signal != "" || exit.Code == nil {
		return guestproto.Response{}, errGuestTransport
	}
	if *exit.Code == 3 {
		return guestproto.Response{}, errGuestTransport
	}
	if *exit.Code != 0 && *exit.Code != 1 {
		return guestproto.Response{}, fmt.Errorf("%w: unsupported ctl exit %d", errGuestControlProtocol, *exit.Code)
	}
	if len(stdout.bytes()) == 0 {
		return guestproto.Response{}, errGuestTransport
	}
	if stdout.truncated {
		return guestproto.Response{}, fmt.Errorf("%w: stdout exceeded frame limit", errGuestControlProtocol)
	}
	responseBytes := stdout.bytes()
	if len(responseBytes) != 0 && responseBytes[len(responseBytes)-1] == '\n' {
		responseBytes = responseBytes[:len(responseBytes)-1]
	}
	if len(responseBytes) == 0 || !bytes.Equal(responseBytes, bytes.TrimSpace(responseBytes)) || bytes.ContainsAny(responseBytes, "\r\n") {
		diagnostic := terminal.SanitizeN(string(stderr.bytes()), 512)
		return guestproto.Response{}, fmt.Errorf("%w: invalid compact JSON framing (%d stdout bytes, stderr %q)", errGuestControlProtocol, len(responseBytes), diagnostic)
	}
	response, decodeErr := guestproto.DecodeResponse(responseBytes)
	if decodeErr != nil {
		return guestproto.Response{}, fmt.Errorf("%w: decode response: %v", errGuestControlProtocol, decodeErr)
	}
	if response.RequestID != requestID {
		return guestproto.Response{}, errGuestTransport
	}
	if response.OK && *exit.Code != 0 || !response.OK && *exit.Code != 1 {
		return guestproto.Response{}, fmt.Errorf("%w: response/exit mismatch", errGuestControlProtocol)
	}
	return response, nil
}

func (client *GuestClient) request(ctx context.Context, operation guestproto.Operation, target string, generation *uint64, key string, params any) (guestproto.Request, []byte, error) {
	deadlineMS, err := client.deadlineMS(ctx)
	if err != nil {
		return guestproto.Request{}, nil, err
	}
	requestID, err := client.nextUUID()
	if err != nil {
		return guestproto.Request{}, nil, err
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return guestproto.Request{}, nil, model.NewError(model.CodeInternal, "encode guest request", nil)
	}
	request := guestproto.Request{
		Protocol:       guestproto.ProtocolV1,
		RequestID:      requestID,
		Operation:      operation,
		Target:         target,
		IfGeneration:   generation,
		IdempotencyKey: key,
		DeadlineMS:     deadlineMS,
		Params:         rawParams,
	}
	encoded, err := guestproto.EncodeRequest(request)
	if err != nil {
		return guestproto.Request{}, nil, model.NewError(model.CodeInvalidInput, "guest request exceeds protocol limits", nil)
	}
	return request, encoded, nil
}

func (client *GuestClient) callContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, model.NewError(model.CodeInvalidInput, "guest request context is nil", nil)
	}
	if err := contextGuestError(ctx); err != nil {
		return nil, nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, client.requestTimeout)
	return callContext, cancel, nil
}

func (client *GuestClient) deadlineMS(ctx context.Context) (uint32, error) {
	if err := contextGuestError(ctx); err != nil {
		return 0, err
	}
	deadline, found := ctx.Deadline()
	if !found {
		return uint32(guestproto.MaxDeadlineMS), nil
	}
	remaining := deadline.Sub(client.now())
	if remaining <= 0 {
		return 0, model.NewError(model.CodeUnavailable, "guest request deadline exceeded", context.DeadlineExceeded)
	}
	milliseconds := (remaining + time.Millisecond - 1) / time.Millisecond
	if milliseconds > guestproto.MaxDeadlineMS {
		milliseconds = guestproto.MaxDeadlineMS
	}
	if milliseconds < 1 {
		milliseconds = 1
	}
	return uint32(milliseconds), nil
}

func (client *GuestClient) validateWorkspace(workspace runtime.ResourceSnapshot) error {
	if client == nil || client.adapter == nil {
		return model.NewError(model.CodeInternal, "guest client is not configured", nil)
	}
	if workspace.ID == "" || workspace.Name == "" || workspace.Kind != runtime.ResourceWorkspace {
		return model.NewError(model.CodeInvalidInput, "guest workspace snapshot is invalid", nil)
	}
	return nil
}

func (client *GuestClient) nextUUID() (string, error) {
	if client == nil || client.uuid == nil {
		return "", model.NewError(model.CodeInternal, "guest UUID policy is not configured", nil)
	}
	value, err := client.uuid()
	if err != nil || !guestproto.ValidUUID(value) {
		return "", model.NewError(model.CodeInternal, "generate guest request identity", nil)
	}
	return value, nil
}

func (client *GuestClient) observeInstance(workspace runtime.ResourceID, instance string) bool {
	client.serverMu.Lock()
	defer client.serverMu.Unlock()
	previous := client.serverInstances[workspace]
	client.serverInstances[workspace] = instance
	return previous != "" && previous != instance
}

func (client *GuestClient) validateStatus(status guestproto.StatusResult) error {
	if status.Processes == nil || len(status.Processes) > guestproto.MaxProcesses {
		return model.NewError(model.CodeUnavailable, "guest returned an invalid status result", nil)
	}
	seen := make(map[string]struct{}, len(status.Processes))
	for _, process := range status.Processes {
		parsed, err := model.ParseSandboxName(process.ID)
		if err != nil || string(parsed) != process.ID || process.Generation != status.Generation || !validGuestState(process.State) || len(process.Failure) > guestproto.MaxStringBytes || len(process.Log) > client.logLimitBytes {
			return model.NewError(model.CodeUnavailable, "guest returned an invalid status result", nil)
		}
		if _, duplicate := seen[process.ID]; duplicate {
			return model.NewError(model.CodeUnavailable, "guest returned an invalid status result", nil)
		}
		seen[process.ID] = struct{}{}
		if process.Exit != nil {
			if err := validateGuestExit(*process.Exit); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeGuestResponse(response guestproto.Response, result any) error {
	if !response.OK {
		return mapGuestProtocolError(response.Error.Code)
	}
	if len(response.Result) == 0 {
		return model.NewError(model.CodeUnavailable, "guest returned an invalid result", nil)
	}
	if err := guestproto.DecodeParams(response.Result, result); err != nil {
		return model.NewError(model.CodeUnavailable, "guest returned an invalid result", nil)
	}
	return nil
}

func mapGuestProtocolError(code guestproto.ErrorCode) error {
	switch code {
	case guestproto.CodeInvalidRequest:
		return model.NewError(model.CodeInvalidInput, "guest rejected the request as invalid", nil)
	case guestproto.CodeUnsupportedProtocol:
		return model.NewError(model.CodeUnavailable, "guest protocol version is unsupported", nil)
	case guestproto.CodeNotFound:
		return model.NewError(model.CodeConflict, "configured guest process was not found", nil)
	case guestproto.CodeWrongState:
		return model.NewError(model.CodeConflict, "guest process is in the wrong state", nil)
	case guestproto.CodeGenerationConflict:
		return model.NewError(model.CodeConflict, "guest generation changed", nil)
	case guestproto.CodeIdempotencyConflict:
		return model.NewError(model.CodeConflict, "guest idempotency key conflicts with another request", nil)
	case guestproto.CodeDeadlineExceeded:
		return model.NewError(model.CodeUnavailable, "guest request deadline exceeded", context.DeadlineExceeded)
	case guestproto.CodePermissionDenied:
		return model.NewError(model.CodeUnavailable, "guest control permission was denied", nil)
	case guestproto.CodeShuttingDown:
		return model.NewError(model.CodeUnavailable, "guest helper is shutting down", nil)
	case guestproto.CodeStartFailed, guestproto.CodeSignalFailed, guestproto.CodeResizeFailed:
		return model.NewError(model.CodeUnavailable, "guest process operation failed", nil)
	default:
		return model.NewError(model.CodeUnavailable, "guest helper failed internally", nil)
	}
}

func validateGuestExit(exit guestproto.ExitStatus) error {
	if (exit.Code == nil) == (exit.Signal == "") {
		return model.NewError(model.CodeUnavailable, "guest returned an invalid exit result", nil)
	}
	if exit.Signal != "" && !validGuestExitSignal(exit.Signal) {
		return model.NewError(model.CodeUnavailable, "guest returned an invalid exit result", nil)
	}
	return nil
}

func validGuestExitSignal(signal string) bool {
	switch signal {
	case "TERM", "INT", "KILL", "HUP", "QUIT":
		return true
	default:
		return false
	}
}

func validGuestState(state guestproto.ProcessState) bool {
	switch state {
	case guestproto.StateConfigured, guestproto.StateWaitingDependencies, guestproto.StateStarting, guestproto.StateRunning, guestproto.StateReady, guestproto.StateUnhealthy, guestproto.StateStopping, guestproto.StateExited, guestproto.StateFailed:
		return true
	default:
		return false
	}
}

func allowedGuestSignal(signal string) bool {
	switch signal {
	case "SIGHUP", "SIGINT", "SIGKILL", "SIGTERM":
		return true
	default:
		return false
	}
}

var (
	errGuestTransport       = errors.New("guest ctl transport failed")
	errGuestControlProtocol = errors.New("guest ctl protocol failed")
)

func guestTransportError() error {
	return model.NewError(model.CodeUnavailable, "guest control transport is unavailable", nil)
}

func guestControlProtocolError(cause error) error {
	return model.NewError(model.CodeUnavailable, cause.Error(), nil)
}

func contextGuestError(ctx context.Context) error {
	if ctx == nil {
		return model.NewError(model.CodeInvalidInput, "guest request context is nil", nil)
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return model.NewError(model.CodeUnavailable, "guest request deadline exceeded", context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return model.NewError(model.CodeUnavailable, "guest request canceled", context.Canceled)
	default:
		return nil
	}
}

type guestCapture struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newGuestCapture(limit int) *guestCapture {
	return &guestCapture{limit: limit}
}

func (capture *guestCapture) Write(value []byte) (int, error) {
	original := len(value)
	remaining := capture.limit - capture.buffer.Len()
	if remaining <= 0 {
		capture.truncated = capture.truncated || original != 0
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		capture.truncated = true
	}
	_, err := capture.buffer.Write(value)
	return original, err
}

func (capture *guestCapture) bytes() []byte {
	return capture.buffer.Bytes()
}

func newGuestUUID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded[:]), nil
}
