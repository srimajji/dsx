package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/srimajji/dsx/internal/guestproto"
	"golang.org/x/sys/unix"
)

var errProducerOutputLimit = errors.New("producer output exceeds size limit")

// ProduceRunFile executes one structured command and streams its stdout into a
// new private result file. The file never grows beyond maximumBytes; an
// oversized producer is killed and its partial output is unlinked.
func ProduceRunFile(ctx context.Context, name string, maximumBytes int64, command guestproto.CommandSpec) error {
	if ctx == nil {
		return errors.New("producer context is required")
	}
	components, err := authorizedRunComponents(name, false)
	if err != nil || !authorizedExportComponents(components, ExportResult) {
		return errors.New("guest result production path is not authorized")
	}
	if maximumBytes < 1 || maximumBytes >= MaxGuestExportBytes {
		return errors.New("guest result production size limit is invalid")
	}
	if err := command.Validate(); err != nil {
		return fmt.Errorf("validate producer command: %w", err)
	}

	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	parents := components[:len(components)-1]
	leaf := components[len(components)-1]
	chain, err := openDirectoryChain(tmpFD, parents, false, uid, gid)
	if err != nil {
		return err
	}
	defer chain.close()
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	parentFD := chain[len(chain)-1].fd
	fd, err := unix.Openat(parentFD, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create guest result file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(parentFD, leaf, 0)
		return errors.New("create guest result file: invalid descriptor")
	}
	complete := false
	closed := false
	var created unix.Stat_t
	createdKnown := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !complete && (!createdKnown || verifyLinkedExportFile(parentFD, leaf, created) == nil) {
			_ = unix.Unlinkat(parentFD, leaf, 0)
		}
	}()
	if err := unix.Fstat(fd, &created); err != nil {
		return fmt.Errorf("inspect new guest result file: %w", err)
	}
	createdKnown = true
	if err := validateExportFile(created, uid, gid, maximumBytes); err != nil {
		return err
	}

	process := exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	process.Dir = command.Cwd
	process.Env = append([]string(nil), os.Environ()...)
	process.Stderr = os.Stderr
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error {
		if process.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open producer stdout: %w", err)
	}
	if err := process.Start(); err != nil {
		return fmt.Errorf("start result producer: %w", err)
	}
	writer := &producerFileWriter{file: file, remaining: maximumBytes}
	_, copyErr := io.Copy(writer, stdout)
	if copyErr != nil {
		_ = process.Cancel()
	}
	waitErr := process.Wait()
	if copyErr != nil || waitErr != nil {
		if errors.Is(copyErr, errProducerOutputLimit) {
			return fmt.Errorf("result producer exceeded %d bytes: %w", maximumBytes, errProducerOutputLimit)
		}
		return fmt.Errorf("result producer failed: %w", errors.Join(copyErr, waitErr))
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync guest result file: %w", err)
	}
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return fmt.Errorf("inspect produced guest result: %w", err)
	}
	if err := validateExportFile(metadata, uid, gid, maximumBytes); err != nil {
		return err
	}
	if err := verifyLinkedExportFile(parentFD, leaf, metadata); err != nil {
		return err
	}
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close guest result file: %w", err)
	}
	closed = true
	complete = true
	return nil
}

type producerFileWriter struct {
	file      *os.File
	remaining int64
}

func (writer *producerFileWriter) Write(value []byte) (int, error) {
	if writer.remaining == 0 {
		return 0, errProducerOutputLimit
	}
	toWrite := value
	exceeded := int64(len(toWrite)) > writer.remaining
	if exceeded {
		toWrite = toWrite[:writer.remaining]
	}
	written, err := writer.file.Write(toWrite)
	writer.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != len(toWrite) {
		return written, io.ErrShortWrite
	}
	if exceeded {
		return written, errProducerOutputLimit
	}
	return written, nil
}
