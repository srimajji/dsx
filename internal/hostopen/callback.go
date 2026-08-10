package hostopen

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxCallbackLease       = 30 * time.Minute
	maxCallbackRequestLine = 8 << 10
	maxCallbackHeaders     = 8 << 10
	maxCallbackQuery       = 4 << 10
	maxCallbackBody        = 1 << 10
)

var callbackErrorLog = log.New(io.Discard, "", 0)

// CallbackResult contains the bounded callback query after the verified state
// parameter has been removed. Callers must treat every remaining value as a
// secret and must not log or render it.
type CallbackResult struct {
	Query url.Values
}

// CallbackLease owns one temporary IPv4 loopback HTTP callback listener.
// Close is idempotent and releases the listener without a PID or a background
// process.
type CallbackLease struct {
	callbackURL string
	state       string
	path        string
	host        string

	listener  net.Listener
	server    *http.Server
	ctx       context.Context
	cancel    context.CancelFunc
	outcome   chan callbackOutcome
	serveDone chan struct{}

	accepted  atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

type callbackOutcome struct {
	result CallbackResult
	err    error
}

// StartCallback binds 127.0.0.1 on a dynamic port and starts a callback lease.
// The returned URL intentionally omits state; State must be supplied as the
// OAuth authorization request's state parameter and the provider must return
// it unchanged to the callback URL.
func StartCallback(parent context.Context, leaseDuration time.Duration) (*CallbackLease, error) {
	if parent == nil {
		return nil, errors.New("OAuth callback context is required")
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if leaseDuration <= 0 || leaseDuration > maxCallbackLease {
		return nil, fmt.Errorf("OAuth callback lease must be greater than zero and at most %s", maxCallbackLease)
	}

	state, err := callbackToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate OAuth callback state: %w", err)
	}
	pathToken, err := callbackToken(18)
	if err != nil {
		return nil, fmt.Errorf("generate OAuth callback path: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind OAuth callback listener: %w", err)
	}

	leaseCtx, cancel := context.WithTimeout(parent, leaseDuration)
	host := listener.Addr().String()
	lease := &CallbackLease{
		callbackURL: "http://" + host + "/oauth/callback/" + pathToken,
		state:       state,
		path:        "/oauth/callback/" + pathToken,
		host:        host,
		listener:    listener,
		ctx:         leaseCtx,
		cancel:      cancel,
		outcome:     make(chan callbackOutcome, 1),
		serveDone:   make(chan struct{}),
	}
	lease.server = &http.Server{
		Handler:                      lease,
		DisableGeneralOptionsHandler: true,
		MaxHeaderBytes:               maxCallbackHeaders,
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  5 * time.Second,
		WriteTimeout:                 5 * time.Second,
		IdleTimeout:                  5 * time.Second,
		ErrorLog:                     callbackErrorLog,
	}
	go lease.serve()
	go func() {
		<-leaseCtx.Done()
		_ = lease.Close()
	}()
	return lease, nil
}

func callbackToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// URL returns the exact loopback callback URL for this lease.
func (lease *CallbackLease) URL() string {
	if lease == nil {
		return ""
	}
	return lease.callbackURL
}

// State returns the cryptographically random state that the provider must
// return exactly once to this lease.
func (lease *CallbackLease) State() string {
	if lease == nil {
		return ""
	}
	return lease.state
}

// Wait waits for the one accepted callback, the caller context, or lease
// shutdown. The returned query never contains the verified state parameter.
func (lease *CallbackLease) Wait(ctx context.Context) (CallbackResult, error) {
	if lease == nil || lease.ctx == nil {
		return CallbackResult{}, errors.New("OAuth callback lease is unavailable")
	}
	if ctx == nil {
		return CallbackResult{}, errors.New("OAuth callback wait context is required")
	}
	select {
	case outcome := <-lease.outcome:
		return outcome.result, outcome.err
	default:
	}
	select {
	case outcome := <-lease.outcome:
		return outcome.result, outcome.err
	case <-ctx.Done():
		_ = lease.Close()
		return CallbackResult{}, ctx.Err()
	case <-lease.ctx.Done():
		_ = lease.Close()
		select {
		case outcome := <-lease.outcome:
			return outcome.result, outcome.err
		default:
			return CallbackResult{}, lease.ctx.Err()
		}
	}
}

// Close releases the listener and all accepted connections. It is safe to call
// concurrently and more than once.
func (lease *CallbackLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.closeOnce.Do(func() {
		lease.cancel()
		listenerErr := lease.listener.Close()
		serverErr := lease.server.Close()
		<-lease.serveDone
		if listenerErr != nil && !errors.Is(listenerErr, net.ErrClosed) {
			lease.closeErr = errors.Join(lease.closeErr, listenerErr)
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) && !errors.Is(serverErr, net.ErrClosed) {
			lease.closeErr = errors.Join(lease.closeErr, serverErr)
		}
	})
	return lease.closeErr
}

func (lease *CallbackLease) serve() {
	defer close(lease.serveDone)
	err := lease.server.Serve(lease.listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return
	}
	select {
	case lease.outcome <- callbackOutcome{err: fmt.Errorf("serve OAuth callback: %w", err)}:
	default:
	}
	lease.cancel()
}

func (lease *CallbackLease) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Connection", "close")
	response.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Content-Type-Options", "nosniff")

	if requestLineSize(request) > maxCallbackRequestLine {
		callbackResponse(response, http.StatusRequestURITooLong, "Callback request rejected.")
		return
	}
	if headerSize(request) > maxCallbackHeaders {
		callbackResponse(response, http.StatusRequestHeaderFieldsTooLarge, "Callback request rejected.")
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		callbackResponse(response, http.StatusMethodNotAllowed, "Callback request rejected.")
		return
	}
	if request.URL.Path != lease.path || request.URL.RawPath != "" || request.Host != lease.host {
		callbackResponse(response, http.StatusNotFound, "Callback request rejected.")
		return
	}
	if len(request.URL.RawQuery) > maxCallbackQuery {
		callbackResponse(response, http.StatusRequestURITooLong, "Callback request rejected.")
		return
	}
	if request.ContentLength > maxCallbackBody {
		callbackResponse(response, http.StatusRequestEntityTooLarge, "Callback request rejected.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCallbackBody+1))
	if err != nil {
		callbackResponse(response, http.StatusBadRequest, "Callback request rejected.")
		return
	}
	if len(body) > maxCallbackBody {
		callbackResponse(response, http.StatusRequestEntityTooLarge, "Callback request rejected.")
		return
	}
	if len(body) != 0 {
		callbackResponse(response, http.StatusBadRequest, "Callback request rejected.")
		return
	}

	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		callbackResponse(response, http.StatusBadRequest, "Callback request rejected.")
		return
	}
	states, found := query["state"]
	if !found || len(states) != 1 || !sameCallbackState(states[0], lease.state) {
		callbackResponse(response, http.StatusForbidden, "Callback request rejected.")
		return
	}
	if !lease.accepted.CompareAndSwap(false, true) {
		callbackResponse(response, http.StatusConflict, "Callback already received.")
		return
	}

	query.Del("state")
	callbackResponse(response, http.StatusOK, "Authentication callback received. Return to the terminal.")
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
	select {
	case lease.outcome <- callbackOutcome{result: CallbackResult{Query: query}}:
	default:
	}
}

func sameCallbackState(candidate, expected string) bool {
	candidateHash := sha256.Sum256([]byte(candidate))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(candidateHash[:], expectedHash[:]) == 1
}

func requestLineSize(request *http.Request) int {
	return len(request.Method) + 1 + len(request.RequestURI) + 1 + len(request.Proto) + 2
}

func headerSize(request *http.Request) int {
	size := len("Host") + 2 + len(request.Host) + 2
	for name, values := range request.Header {
		for _, value := range values {
			size += len(name) + 2 + len(value) + 2
		}
	}
	return size + 2
}

func callbackResponse(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, message+"\n")
}
