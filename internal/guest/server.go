package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/guestproto"
)

const (
	controlDirectoryMode = 0o750
	controlSocketMode    = 0o660
	maxConnections       = 32
)

type ServerOptions struct {
	Path        string
	ExpectedUID *uint32
	ExpectedGID *uint32
}

type Server struct {
	supervisor *Supervisor
	path       string
	uid        uint32
	ownerUID   uint32
	gid        uint32
	ready      chan struct{}
	readyOnce  sync.Once
	listener   *net.UnixListener
	createdDir bool
	socketInfo os.FileInfo
	workers    sync.WaitGroup
}

func NewServer(supervisor *Supervisor, options ServerOptions) (*Server, error) {
	if supervisor == nil {
		return nil, errors.New("supervisor is required")
	}
	path := options.Path
	if path == "" {
		path = DefaultSocketPath
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("control socket path must be clean and absolute")
	}
	uid := supervisor.childUID
	gid := supervisor.childGID
	if options.ExpectedUID != nil && *options.ExpectedUID != uid {
		return nil, errors.New("server UID does not match supervisor child UID")
	}
	if options.ExpectedGID != nil && *options.ExpectedGID != gid {
		return nil, errors.New("server GID does not match supervisor child GID")
	}
	return &Server{supervisor: supervisor, path: path, uid: uid, gid: gid, ownerUID: uint32(os.Geteuid()), ready: make(chan struct{})}, nil
}

func (server *Server) Ready() <-chan struct{} { return server.ready }

func (server *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listener, err := server.listen()
	if err != nil {
		return err
	}
	server.listener = listener
	server.readyOnce.Do(func() { close(server.ready) })
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-server.supervisor.Done():
		case <-stop:
			return
		}
		_ = listener.Close()
	}()
	defer close(stop)
	defer server.cleanup()

	semaphore := make(chan struct{}, maxConnections)
	for {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			if ctx.Err() != nil || channelClosed(server.supervisor.Done()) || errors.Is(acceptErr, net.ErrClosed) {
				server.workers.Wait()
				return nil
			}
			return fmt.Errorf("accept control connection: %w", acceptErr)
		}
		select {
		case semaphore <- struct{}{}:
			server.workers.Add(1)
			go func() {
				defer func() { <-semaphore; server.workers.Done() }()
				server.handleConnection(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (server *Server) listen() (*net.UnixListener, error) {
	parent := filepath.Dir(server.path)
	if err := validateControlAncestorChain(filepath.Dir(parent), server.ownerUID); err != nil {
		return nil, fmt.Errorf("verify control directory ancestors: %w", err)
	}
	if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, controlDirectoryMode); err != nil {
			return nil, fmt.Errorf("create control directory: %w", err)
		}
		server.createdDir = true
		if err := os.Chmod(parent, controlDirectoryMode); err != nil {
			return nil, fmt.Errorf("set control directory permissions: %w", err)
		}
		if err := os.Chown(parent, int(server.ownerUID), int(server.gid)); err != nil {
			return nil, fmt.Errorf("set root-owned control directory: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect control directory: %w", err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("verify control directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("control directory is not a real directory")
	}
	uid, gid, ok := fileOwnership(info)
	if !ok || uid != server.ownerUID || gid != server.gid {
		return nil, errors.New("control directory ownership is unsafe")
	}
	if info.Mode().Perm() != controlDirectoryMode {
		return nil, fmt.Errorf("control directory mode must be %04o", controlDirectoryMode)
	}
	if err := validateControlAncestorChain(parent, server.ownerUID); err != nil {
		return nil, fmt.Errorf("verify control directory chain: %w", err)
	}
	if socketInfo, err := os.Lstat(server.path); err == nil {
		if err := server.removeStaleSocket(socketInfo); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect control socket path: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: server.path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = listener.Close()
			_ = os.Remove(server.path)
		}
	}()
	if err := os.Chmod(server.path, controlSocketMode); err != nil {
		return nil, fmt.Errorf("set control socket permissions: %w", err)
	}
	if err := os.Chown(server.path, int(server.ownerUID), int(server.gid)); err != nil {
		return nil, fmt.Errorf("set root-owned control socket: %w", err)
	}
	socketInfo, err := os.Lstat(server.path)
	if err != nil {
		return nil, errors.New("control socket permissions could not be verified")
	}
	socketUID, socketGID, ownershipOK := fileOwnership(socketInfo)
	if socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != controlSocketMode || !ownershipOK || socketUID != server.ownerUID || socketGID != server.gid {
		return nil, errors.New("control socket permissions could not be verified")
	}
	server.socketInfo = socketInfo
	failed = false
	return listener, nil
}

func validateControlAncestorChain(target string, ownerUID uint32) error {
	if ownerUID != 0 {
		return nil
	}
	target = filepath.Clean(target)
	if !filepath.IsAbs(target) {
		return errors.New("control directory chain is not absolute")
	}
	current := string(filepath.Separator)
	relative, err := filepath.Rel(current, target)
	if err != nil {
		return err
	}
	components := append([]string{current}, splitPathComponents(relative)...)
	for _, component := range components {
		if component != current {
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		uid, _, owned := fileOwnership(info)
		if !owned || uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe control ancestor %s", current)
		}
	}
	return nil
}

func splitPathComponents(value string) []string {
	var components []string
	for value != "." && value != string(filepath.Separator) && value != "" {
		directory, base := filepath.Split(value)
		if base != "" {
			components = append([]string{base}, components...)
		}
		value = filepath.Clean(directory)
	}
	return components
}

func (server *Server) removeStaleSocket(observed os.FileInfo) error {
	uid, gid, owned := fileOwnership(observed)
	if observed.Mode()&os.ModeSocket == 0 || observed.Mode().Perm() != controlSocketMode ||
		!owned || uid != server.ownerUID || gid != server.gid {
		return errors.New("control socket path already exists and is not a verified DSX socket")
	}
	connection, err := net.DialTimeout("unix", server.path, 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return errors.New("control socket already has an active listener")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("verify stale control socket: %w", err)
	}
	current, statErr := os.Lstat(server.path)
	if statErr != nil {
		return fmt.Errorf("recheck stale control socket: %w", statErr)
	}
	if !os.SameFile(observed, current) {
		return errors.New("control socket changed during stale-socket verification")
	}
	if err := os.Remove(server.path); err != nil {
		return fmt.Errorf("remove verified stale control socket: %w", err)
	}
	return nil
}

func (server *Server) handleConnection(connection *net.UnixConn) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Duration(guestproto.MaxDeadlineMS) * time.Millisecond)); err != nil {
		return

	}
	uid, gid, err := peerCredentials(connection)
	if err != nil || uid != server.uid || gid != server.gid {
		return
	}
	frame, err := guestproto.ReadFrame(connection)
	if err != nil {
		return
	}
	request, err := guestproto.DecodeRequest(frame)
	if err != nil {
		if guestproto.ValidUUID(request.RequestID) {
			response := server.supervisor.errorResponse(request.RequestID, guestproto.ErrorCodeOf(err), "invalid request", nil)
			_ = guestproto.WriteResponse(connection, response)
		}
		return
	}
	deadline := time.Now().Add(time.Duration(request.DeadlineMS) * time.Millisecond)
	if err := connection.SetDeadline(deadline); err != nil {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	response := server.supervisor.Handle(ctx, request)
	_ = guestproto.WriteResponse(connection, response)
}

func (server *Server) cleanup() {
	if server.listener != nil {
		_ = server.listener.Close()
	}
	if server.socketInfo != nil {
		if current, err := os.Lstat(server.path); err == nil && os.SameFile(current, server.socketInfo) {
			_ = os.Remove(server.path)
		}
	}
	if server.createdDir {
		_ = os.Remove(filepath.Dir(server.path))
	}
}

func fileOwnership(info os.FileInfo) (uint32, uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}
