package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	maxLoginCallbackTimeout       = 30 * time.Minute
	maxLoginProviderOutput        = 64 << 10
	maxLoginProviderURL           = 8 << 10
	claudeOAuthPinnedVersion      = "2.1.226"
	claudeOAuthHost               = "claude.com"
	claudeOAuthPath               = "/cai/oauth/authorize"
	claudeOAuthClientID           = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthRedirectURI        = "https://platform.claude.com/oauth/code/callback"
	claudeOAuthScope              = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	claudeOAuthQueryRedaction     = "?[oauth-query-redacted]"
	claudeOAuthDynamicValueLength = 43
)

var providerURLPattern = regexp.MustCompile(`https://[^\s<>"'\\]+`)

// LoginBrowserOpener opens one exact provider URL with a timeout-scoped lease.
type LoginBrowserOpener func(context.Context, string) error

type HarnessLoginRequest struct {
	Root           string
	ApproveConfig  string
	Agent          string
	Profile        string
	Interactive    bool
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	RunInteractive InteractiveChildRunner
	BeforeExec     func(HarnessLoginResult) error
	OpenBrowser    LoginBrowserOpener
}

type HarnessLoginResult struct {
	Agent         harness.Name
	Version       string
	Exit          runtime.Exit
	AuthPromotion auth.Promotion
}

// Login is the only application route that may initiate a provider login. It
// requires an explicitly approved plan and an interactive terminal; ordinary
// harness execution never starts a login implicitly.
func (service *HarnessService) Login(ctx context.Context, request HarnessLoginRequest) (result HarnessLoginResult, returnErr error) {
	if ctx == nil {
		return result, model.NewError(model.CodeInvalidInput, "login context is nil", nil)
	}
	if service == nil || service.lifecycle == nil || service.auth == nil {
		return result, model.NewError(model.CodeUnavailable, "harness service is unavailable", nil)
	}
	if !request.Interactive || request.RunInteractive == nil || request.Stdin == nil || request.Stdout == nil || request.Stderr == nil {
		return result, model.NewError(model.CodeInvalidInput, "login requires an interactive terminal", nil)
	}
	name, err := harness.ParseName(request.Agent)
	if err != nil {
		return result, model.NewError(model.CodeInvalidInput, err.Error(), nil)
	}
	profileName := request.Profile
	if _, err := model.ParseSandboxName(profileName); err != nil {
		return result, model.NewError(model.CodeInvalidInput, "invalid authentication profile", err)
	}
	adapter := service.adapters[name]
	if adapter == nil {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("harness %q is not installed", name), nil)
	}
	layout := adapter.AuthLayout()
	if err := harness.ValidateAuthLayout(layout); err != nil {
		return result, model.Wrap(model.CodeUnavailable, "validate harness authentication layout", err)
	}

	// Resolve and authorize the exact selected-agent plan before creating a
	// sandbox, authentication generation, or run copy.
	inspected, err := service.lifecycle.inspectApproved(ctx, StartRequest{
		Root: request.Root, ApproveConfig: request.ApproveConfig, Agent: string(name),
	})
	if err != nil {
		return result, err
	}
	persistence, err := authorizeHarnessGrant(inspected.Plan, name, profileName)
	if err != nil {
		return result, err
	}

	sessionID, err := service.lifecycle.newRunID(service.lifecycle.now().UTC())
	if err != nil {
		return result, model.Wrap(model.CodeInternal, "generate login session ID", err)
	}
	roots := harnessRoots(sessionID)
	if diagnostics, err := adapter.Preflight(ctx, roots); err != nil {
		return result, model.Wrap(model.CodeUnavailable, "harness login preflight", err)
	} else {
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == "error" {
				return result, model.NewError(model.CodeUnavailable, "harness login preflight failed: "+diagnostic.Code, nil)
			}
		}
	}

	started, err := service.lifecycle.Start(ctx, StartRequest{
		Root: request.Root, ApproveConfig: request.ApproveConfig, Agent: string(name),
	})
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, started.hostBridges.Close()) }()

	snapshot, current, err := service.workspace(ctx, request.Root, started.ProjectID, string(name))
	if err != nil {
		return result, err
	}
	currentPersistence, err := authorizeHarnessGrant(current, name, profileName)
	if err != nil {
		return result, err
	}
	if current.ExecutableHash != inspected.Plan.ExecutableHash || currentPersistence != persistence {
		return result, model.NewError(model.CodeUnapproved, "harness login authority changed after approval", nil)
	}
	if err := service.verifyHarnessBuildAttestation(ctx, snapshot, current, adapter, func(stdout, stderr io.Writer) (runtime.Exit, error) {
		return service.shell(ctx, request.Root, string(name), []string{"/bin/cat", "--", harness.BuildAttestationPath}, nil, false, nil, stdout, stderr, nil)
	}); err != nil {
		return result, err
	}
	mcpRequest := harness.MCPRequest{Roots: roots}
	injection, err := adapter.EphemeralMCP(mcpRequest)
	if err != nil {
		return result, model.Wrap(model.CodeUnavailable, "prepare isolated harness login MCP configuration", err)
	}
	profile := auth.Profile{Harness: name, Name: profileName}
	if persistence == "sandbox" {
		profile = auth.SandboxProfile(profile, started.ProjectID, string(current.Sandbox.Name))
	}
	var runCopy auth.Copy
	if persistence == "global" {
		if _, err := service.auth.Ensure(ctx, profile, adapter); err != nil {
			return result, model.Wrap(model.CodeUnavailable, "ensure authentication profile", err)
		}
		runCopy, err = service.auth.PrepareGlobalSandbox(ctx, profile, sessionID, started.ProjectID, string(current.Sandbox.Name), adapter)
	} else {
		runCopy, err = service.auth.PrepareSandbox(ctx, profile, sessionID, adapter)
	}
	if err != nil {
		return result, model.Wrap(model.CodeUnavailable, "prepare login authentication copy", err)
	}
	cleanupBase := context.WithoutCancel(ctx)
	defer func() {
		guestCleanupCtx, cancelGuestCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
		_, guestCleanupErr := service.shell(guestCleanupCtx, request.Root, string(name), []string{"/bin/rm", "-rf", "--", path.Dir(roots.Home)}, nil, false, nil, nil, nil, nil)
		cancelGuestCleanup()
		copyCleanupCtx, cancelCopyCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
		copyCleanupErr := service.auth.RemoveRun(copyCleanupCtx, runCopy)
		cancelCopyCleanup()
		returnErr = errors.Join(returnErr, guestCleanupErr, copyCleanupErr)
	}()
	if err := service.prepareGuestRoots(ctx, snapshot, roots); err != nil {
		return result, err
	}
	if err := service.installGeneratedFiles(ctx, snapshot, runCopy.Root, roots, injection.Files); err != nil {
		return result, err
	}
	if err := service.verifyEffectiveMCP(ctx, request.Root, name, adapter, mcpRequest, injection); err != nil {
		return result, err
	}
	if err := service.copyAuthToGuest(ctx, snapshot, runCopy.Root, roots.Auth, layout); err != nil {
		return result, err
	}
	readOnlyConfig, err := service.copyReadOnlyConfigToGuest(ctx, snapshot, runCopy.ReadOnlyRoot, roots.ReadOnlyConfig, layout)
	if err != nil {
		return result, err
	}
	if len(readOnlyConfig) != 0 {
		defer func() {
			readOnlyCleanupCtx, cancelReadOnlyCleanup := context.WithTimeout(cleanupBase, 30*time.Second)
			defer cancelReadOnlyCleanup()
			returnErr = errors.Join(returnErr, service.removeReadOnlyGuestRoot(readOnlyCleanupCtx, snapshot, roots.ReadOnlyConfig))
		}()
	}
	flow, err := adapter.Login(harness.LoginRequest{Roots: roots, ReadOnlyConfig: readOnlyConfig})
	if err != nil {
		return result, model.Wrap(model.CodeUnavailable, "prepare harness login", err)
	}
	artifact := adapter.Version()
	if err := validateLoginFlow(flow, roots, request.OpenBrowser, name, artifact.Version); err != nil {
		return result, err
	}
	flow.Exec.Argv = insertHarnessArgs(flow.Exec.Argv, injection.Args)
	for key, value := range injection.Env {
		if flow.Exec.Env == nil {
			flow.Exec.Env = make(map[string]string)
		}
		flow.Exec.Env[key] = value
	}
	flow.Exec.Env, err = mergeHostBridgeEnvironment(flow.Exec.Env, started.hostBridges.Environment())
	if err != nil {
		return result, err
	}

	var versionStdout, versionStderr cappedBuffer
	versionStdout.limit = maxHarnessVersionOutput
	versionStderr.limit = maxHarnessVersionOutput
	versionExit, err := service.shell(ctx, request.Root, string(name), []string{artifact.Executable, "--version"}, rootEnvironment(roots, layout), false, nil, &versionStdout, &versionStderr, nil)
	if err != nil {
		return result, err
	}
	if versionExit.Code == nil || *versionExit.Code != 0 {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("%s version command failed", name), nil)
	}
	if err := adapter.ValidateVersion(versionStdout.String(), versionStderr.String()); err != nil {
		return result, model.Wrap(model.CodeUnavailable, "validate pinned harness version", err)
	}

	result = HarnessLoginResult{Agent: name, Version: artifact.Version}
	if request.BeforeExec != nil {
		if err := request.BeforeExec(result); err != nil {
			return result, model.Wrap(model.CodeInternal, "render harness login status", err)
		}
	}

	loginCtx := ctx
	cancelLogin := func() {}
	if flow.CallbackTimeout > 0 {
		loginCtx, cancelLogin = context.WithTimeout(ctx, time.Duration(flow.CallbackTimeout)*time.Second)
	}
	defer cancelLogin()
	runner := request.RunInteractive
	var capture *loginProviderCapture
	if flow.OpenBrowser {
		capture = newLoginProviderCapture(loginCtx, request.OpenBrowser, cancelLogin)
		defer capture.Close()
		baseRunner := runner
		runner = func(childCtx context.Context, child InteractiveChild) (runtime.Exit, error) {
			child.Stdout = capture.Writer(child.Stdout)
			child.Stderr = capture.Writer(child.Stderr)
			return baseRunner(childCtx, child)
		}
	}
	exit, invocationErr := service.shellWithSecretEnvironment(loginCtx, request.Root, string(name), flow.Exec.Argv, flow.Exec.Env, adapter.RedactionRules().EnvironmentKeys, true, request.Stdin, request.Stdout, request.Stderr, runner)
	result.Exit = exit
	if capture != nil {
		if err := capture.Result(loginCtx); err != nil {
			invocationErr = errors.Join(invocationErr, model.Wrap(model.CodeUnavailable, "open login browser", err))
		}
	}
	if invocationErr != nil {
		return result, invocationErr
	}
	if exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return result, nil
	}
	syncCtx, cancelSync := context.WithTimeout(cleanupBase, 30*time.Second)
	defer cancelSync()
	if err := service.copyAuthFromGuest(syncCtx, snapshot, runCopy, roots.Auth, adapter); err != nil {
		return result, err
	}
	promotion, err := service.auth.Promote(syncCtx, runCopy, adapter)
	result.AuthPromotion = promotion
	if promotion.Conflict {
		err = errors.Join(err, model.NewError(model.CodeConflict, "authentication login conflicted; candidate preserved", nil))
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func validateLoginFlow(flow harness.LoginFlow, roots harness.RunRoots, opener LoginBrowserOpener, name harness.Name, version string) error {
	if err := harness.ValidateExecSpec(flow.Exec); err != nil {
		return model.Wrap(model.CodeUnavailable, "validate harness login flow", err)
	}
	if !flow.Exec.Terminal || flow.Exec.Cwd != roots.Workspace {
		return model.NewError(model.CodeUnavailable, "harness login flow must own a terminal in the workspace root", nil)
	}
	if flow.CallbackTimeout < 0 || flow.CallbackTimeout > int(maxLoginCallbackTimeout/time.Second) {
		return model.NewError(model.CodeUnavailable, "harness login callback timeout is outside the supported bound", nil)
	}
	if flow.OpenBrowser && (flow.CallbackTimeout == 0 || opener == nil) {
		return model.NewError(model.CodeUnavailable, "harness login requires a configured safe browser opener", nil)
	}
	if flow.OpenBrowser && (name != harness.Claude || version != claudeOAuthPinnedVersion) {
		return model.NewError(model.CodeUnavailable, "browser login is unsupported for this harness artifact", nil)
	}
	if !flow.OpenBrowser && flow.CallbackTimeout != 0 {
		return model.NewError(model.CodeUnavailable, "harness login callback timeout requires browser opening", nil)
	}
	return nil
}

type loginProviderCapture struct {
	mu            sync.Mutex
	writeMu       sync.Mutex
	output        []byte
	observedBytes int
	exhausted     bool
	opened        bool
	openErr       error
	ctx           context.Context
	opener        LoginBrowserOpener
	cancel        context.CancelFunc
	terminalMode  loginTerminalMode
	schemeMatched int
}

type loginTerminalMode uint8

const (
	loginTerminalText loginTerminalMode = iota
	loginTerminalURL
	loginTerminalQuery
)

func newLoginProviderCapture(ctx context.Context, opener LoginBrowserOpener, cancel context.CancelFunc) *loginProviderCapture {
	return &loginProviderCapture{ctx: ctx, opener: opener, cancel: cancel}
}

func (capture *loginProviderCapture) Writer(destination io.Writer) io.Writer {
	if destination == nil {
		return nil
	}
	return &loginCaptureWriter{capture: capture, destination: destination}
}

type loginCaptureWriter struct {
	capture     *loginProviderCapture
	destination io.Writer
}

func (writer *loginCaptureWriter) Write(data []byte) (int, error) {
	writer.capture.writeMu.Lock()
	defer writer.capture.writeMu.Unlock()

	writer.capture.observe(data)
	sanitized := terminal.SanitizeN(string(writer.capture.redactForTerminal(data)), maxLoginProviderOutput)
	if sanitized == "" {
		return len(data), nil
	}
	if _, err := io.WriteString(writer.destination, sanitized); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (capture *loginProviderCapture) observe(data []byte) {
	capture.mu.Lock()
	if capture.opened || capture.exhausted || capture.opener == nil {
		capture.mu.Unlock()
		return
	}
	if len(data) > maxLoginProviderOutput-capture.observedBytes {
		capture.exhausted = true
		clear(capture.output)
		capture.output = nil
		cancel := capture.cancel
		capture.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	capture.observedBytes += len(data)
	capture.output = append(capture.output, data...)
	providerURL := capture.discoverProviderURLLocked(false)
	if providerURL == "" {
		capture.mu.Unlock()
		return
	}
	capture.opened = true
	clear(capture.output)
	capture.output = nil
	opener := capture.opener
	ctx := capture.ctx
	cancel := capture.cancel
	capture.mu.Unlock()

	capture.openProviderURL(ctx, opener, cancel, providerURL)
}

func (capture *loginProviderCapture) discoverProviderURLLocked(final bool) string {
	for _, location := range providerURLPattern.FindAllIndex(capture.output, -1) {
		rawCandidate := string(capture.output[location[0]:location[1]])
		candidate := strings.TrimRight(rawCandidate, ".,;)]}")
		if !final && location[1] == len(capture.output) && len(candidate) == len(rawCandidate) {
			continue
		}
		providerURL, err := canonicalClaudeProviderURL(candidate)
		if err == nil {
			return providerURL
		}
	}
	return ""
}

func (capture *loginProviderCapture) finishDiscovery() {
	capture.mu.Lock()
	if capture.opened || capture.exhausted || capture.opener == nil {
		capture.mu.Unlock()
		return
	}
	providerURL := capture.discoverProviderURLLocked(true)
	if providerURL == "" {
		capture.mu.Unlock()
		return
	}
	capture.opened = true
	clear(capture.output)
	capture.output = nil
	opener := capture.opener
	ctx := capture.ctx
	cancel := capture.cancel
	capture.mu.Unlock()

	capture.openProviderURL(ctx, opener, cancel, providerURL)
}

func (capture *loginProviderCapture) openProviderURL(ctx context.Context, opener LoginBrowserOpener, cancel context.CancelFunc, providerURL string) {
	if err := opener(ctx, providerURL); err != nil {
		capture.mu.Lock()
		capture.openErr = errors.New("provider URL opener failed")
		clear(capture.output)
		capture.output = nil
		capture.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (capture *loginProviderCapture) redactForTerminal(data []byte) []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	const scheme = "://"
	rendered := make([]byte, 0, len(data))
	for _, character := range data {
		switch capture.terminalMode {
		case loginTerminalQuery:
			if isProviderURLDelimiter(character) {
				capture.terminalMode = loginTerminalText
				capture.schemeMatched = 0
				rendered = append(rendered, character)
			}
		case loginTerminalURL:
			if character == '?' {
				rendered = append(rendered, claudeOAuthQueryRedaction...)
				capture.terminalMode = loginTerminalQuery
				continue
			}
			rendered = append(rendered, character)
			if isProviderURLDelimiter(character) {
				capture.terminalMode = loginTerminalText
				capture.schemeMatched = 0
			}
		default:
			rendered = append(rendered, character)
			if character == scheme[capture.schemeMatched] {
				capture.schemeMatched++
				if capture.schemeMatched == len(scheme) {
					capture.terminalMode = loginTerminalURL
					capture.schemeMatched = 0
				}
			} else if character == scheme[0] {
				capture.schemeMatched = 1
			} else {
				capture.schemeMatched = 0
			}
		}
	}
	return rendered
}

func isProviderURLDelimiter(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\v', '\f', '<', '>', '"', '\'', '\\':
		return true
	default:
		return false
	}
}

func (capture *loginProviderCapture) Result(ctx context.Context) error {
	capture.finishDiscovery()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	clear(capture.output)
	capture.output = nil
	if capture.openErr != nil {
		return capture.openErr
	}
	if capture.opened {
		return nil
	}
	if capture.exhausted {
		return fmt.Errorf("Claude login output exceeded the provider URL capture bound")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("timed out waiting for a supported Claude login URL: %w", err)
	}
	return fmt.Errorf("Claude login ended before emitting a supported provider URL")
}

func (capture *loginProviderCapture) Close() {
	capture.writeMu.Lock()
	defer capture.writeMu.Unlock()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	clear(capture.output)
	capture.output = nil
	capture.ctx = nil
	capture.opener = nil
	capture.cancel = nil
}

func validateClaudeProviderURL(value string) error {
	_, err := canonicalClaudeProviderURL(value)
	return err
}

func canonicalClaudeProviderURL(value string) (string, error) {
	if len(value) == 0 || len(value) > maxLoginProviderURL || strings.TrimSpace(value) != value {
		return "", errors.New("provider URL is outside the supported bound")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != claudeOAuthHost || parsed.User != nil ||
		parsed.Fragment != "" || parsed.Opaque != "" || parsed.ForceQuery || parsed.Path != claudeOAuthPath ||
		parsed.RawPath != "" || parsed.EscapedPath() != claudeOAuthPath {
		return "", errors.New("provider URL origin is not allowed")
	}
	for _, field := range strings.Split(parsed.RawQuery, "&") {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) != 2 {
			return "", errors.New("provider URL query encoding is invalid")
		}
		key, keyErr := url.QueryUnescape(pair[0])
		queryValue, valueErr := url.QueryUnescape(pair[1])
		if keyErr != nil || valueErr != nil || pair[0] != url.QueryEscape(key) || pair[1] != url.QueryEscape(queryValue) {
			return "", errors.New("provider URL query encoding is invalid")
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query) != 8 {
		return "", errors.New("provider URL query is invalid")
	}
	expected := map[string]string{
		"code":                  "true",
		"client_id":             claudeOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          claudeOAuthRedirectURI,
		"scope":                 claudeOAuthScope,
		"code_challenge_method": "S256",
	}
	for key, expectedValue := range expected {
		values, found := query[key]
		if !found || len(values) != 1 || values[0] != expectedValue {
			return "", fmt.Errorf("provider URL query key %q is invalid", key)
		}
	}
	for _, key := range []string{"state", "code_challenge"} {
		values, found := query[key]
		if !found || len(values) != 1 || !isClaudeOAuthDynamicValue(values[0]) {
			return "", fmt.Errorf("provider URL query key %q is invalid", key)
		}
	}
	trustedQuery := make(url.Values, len(query))
	for key, values := range query {
		trustedQuery.Set(key, values[0])
	}
	trusted := url.URL{Scheme: "https", Host: claudeOAuthHost, Path: claudeOAuthPath, RawQuery: trustedQuery.Encode()}
	return trusted.String(), nil
}

func isClaudeOAuthDynamicValue(value string) bool {
	if len(value) != claudeOAuthDynamicValueLength {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}
