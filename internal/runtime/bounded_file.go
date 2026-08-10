package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrBoundedFileTooLarge = errors.New("bounded file exceeds size limit")
	ErrBoundedFileNUL      = errors.New("bounded text file contains NUL")
)

type BoundedFileOptions struct {
	MaximumBytes int64
	RejectNUL    bool
	Mode         os.FileMode
}

// ReceiveBoundedFile creates destination exclusively and exposes a streaming
// writer with a hard byte cap. Any stream, sync, close, or metadata failure
// removes the partial file before returning.
func ReceiveBoundedFile(destination HostPath, options BoundedFileOptions, stream func(io.Writer) error) (returnErr error) {
	name := string(destination)
	if name == "" || !filepath.IsAbs(name) || options.MaximumBytes < 1 || options.Mode != 0o600 || stream == nil {
		return errors.New("invalid bounded file destination or options")
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, options.Mode)
	if err != nil {
		return fmt.Errorf("create bounded file: %w", err)
	}
	if err := file.Chmod(options.Mode); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return fmt.Errorf("set bounded file mode: %w", err)
	}
	cleanup := true
	defer func() {
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if cleanup {
			returnErr = errors.Join(returnErr, os.Remove(name))
		}
	}()

	writer := &hardCapWriter{writer: file, remaining: options.MaximumBytes, rejectNUL: options.RejectNUL}
	if err := stream(writer); err != nil {
		return err
	}
	if writer.failure != nil {
		return writer.failure
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync bounded file: %w", err)
	}
	beforeClose, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect bounded file: %w", err)
	}
	if !beforeClose.Mode().IsRegular() || beforeClose.Mode().Perm() != options.Mode || beforeClose.Size() != writer.written || beforeClose.Size() > options.MaximumBytes {
		return errors.New("bounded file metadata does not match transfer")
	}
	if err := file.Close(); err != nil {
		file = nil
		return fmt.Errorf("close bounded file: %w", err)
	}
	file = nil
	linked, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("revalidate bounded file: %w", err)
	}
	if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 || linked.Mode().Perm() != options.Mode || linked.Size() != writer.written || !os.SameFile(beforeClose, linked) {
		return errors.New("bounded file changed during transfer")
	}
	cleanup = false
	return nil
}

type hardCapWriter struct {
	writer    io.Writer
	remaining int64
	written   int64
	rejectNUL bool
	failure   error
}

func (writer *hardCapWriter) Write(contents []byte) (int, error) {
	if writer.failure != nil {
		return 0, writer.failure
	}
	if writer.rejectNUL && bytes.IndexByte(contents, 0) >= 0 {
		writer.failure = ErrBoundedFileNUL
		return 0, writer.failure
	}
	if int64(len(contents)) > writer.remaining {
		writer.failure = ErrBoundedFileTooLarge
		return 0, writer.failure
	}
	written, err := writer.writer.Write(contents)
	writer.remaining -= int64(written)
	writer.written += int64(written)
	if err == nil && written != len(contents) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.failure = err
	}
	return written, err
}
