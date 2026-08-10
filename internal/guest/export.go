package guest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const MaxGuestExportBytes int64 = (512 << 20) + 1

var ErrRunArtifactMissing = errors.New("guest run artifact is absent")

type ExportKind string

const (
	ExportAuth   ExportKind = "auth"
	ExportResult ExportKind = "result"
)

// ExportRunFile streams one exact private run file to output. The file and
// every parent remain anchored by no-follow descriptors for the whole
// operation; paths outside the kind-specific allowlist are never opened.
func ExportRunFile(name string, kind ExportKind, maximumBytes int64, output io.Writer) error {
	components, err := authorizedRunComponents(name, false)
	if err != nil || !authorizedExportComponents(components, kind) {
		return errors.New("guest file export path is not authorized")
	}
	if maximumBytes < 1 || maximumBytes > MaxGuestExportBytes {
		return errors.New("guest file export size limit is invalid")
	}
	if output == nil {
		return errors.New("guest file export output is required")
	}

	tmpFD, err := openTemporaryRoot()
	if err != nil {
		return err
	}
	defer unix.Close(tmpFD)

	parents := components[:len(components)-1]
	leaf := components[len(components)-1]
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	chain, err := openDirectoryChain(tmpFD, parents, false, uid, gid)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrRunArtifactMissing
		}
		return err
	}
	defer chain.close()
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}

	parentFD := chain[len(chain)-1].fd
	var linkedBefore unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &linkedBefore, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrRunArtifactMissing
		}
		return fmt.Errorf("inspect guest export path: %w", err)
	}
	if err := validateExportFile(linkedBefore, uid, gid, maximumBytes); err != nil {
		return err
	}
	fd, err := unix.Openat(parentFD, leaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrRunArtifactMissing
		}
		return fmt.Errorf("open guest export file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open guest export file: invalid descriptor")
	}
	defer file.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return fmt.Errorf("inspect guest export file: %w", err)
	}
	if err := validateExportFile(before, uid, gid, maximumBytes); err != nil {
		return err
	}
	beforeInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect guest export timestamp: %w", err)
	}
	if err := verifyLinkedExportFile(parentFD, leaf, before); err != nil {
		return err
	}

	var destination io.Writer = output
	if kind == ExportAuth {
		destination = nulRejectingWriter{writer: output}
	}
	written, err := io.CopyN(destination, file, before.Size)
	if err != nil || written != before.Size {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("stream guest export file: %w", err)
	}
	var probe [1]byte
	read, err := file.Read(probe[:])
	if read != 0 {
		return errors.New("guest export file grew during transfer")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("recheck guest export length: %w", err)
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return fmt.Errorf("reinspect guest export file: %w", err)
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reinspect guest export timestamp: %w", err)
	}
	if err := validateExportFile(after, uid, gid, maximumBytes); err != nil {
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mode != after.Mode || before.Uid != after.Uid || before.Gid != after.Gid || before.Nlink != after.Nlink || exportMetadataTimeChanged(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		return errors.New("guest export file changed during transfer")
	}
	if err := verifyLinkedExportFile(parentFD, leaf, after); err != nil {
		return err
	}
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	return nil
}

// RemoveRunExportFile unlinks one exact result export after descriptor-safe
// identity and metadata validation. Authentication artifacts are never removed.
func RemoveRunExportFile(name string) error {
	components, err := authorizedRunComponents(name, false)
	if err != nil || !authorizedExportComponents(components, ExportResult) {
		return errors.New("guest result cleanup path is not authorized")
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
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer chain.close()
	parentFD := chain[len(chain)-1].fd
	var linked unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("inspect guest result cleanup path: %w", err)
	}
	if err := validateExportFile(linked, uid, gid, MaxGuestExportBytes); err != nil {
		return err
	}
	fd, err := unix.Openat(parentFD, leaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open guest result cleanup file: %w", err)
	}
	defer unix.Close(fd)
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return fmt.Errorf("inspect guest result cleanup file: %w", err)
	}
	if err := validateExportFile(metadata, uid, gid, MaxGuestExportBytes); err != nil {
		return err
	}
	if err := verifyLinkedExportFile(parentFD, leaf, metadata); err != nil {
		return err
	}
	if err := chain.revalidate(tmpFD); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, leaf, 0); err != nil {
		return fmt.Errorf("remove guest result export: %w", err)
	}
	return nil
}

func authorizedExportComponents(components []string, kind ExportKind) bool {
	if len(components) < 4 {
		return false
	}
	switch kind {
	case ExportAuth:
		return components[2] == "auth"
	case ExportResult:
		if len(components) != 4 || components[2] != "tmp" {
			return false
		}
		leaf := components[3]
		if !strings.HasSuffix(leaf, ".bundle") {
			return false
		}
		indexText, found := strings.CutPrefix(strings.TrimSuffix(leaf, ".bundle"), "result-")
		if !found || indexText == "" {
			return false
		}
		index, err := strconv.ParseUint(indexText, 10, 31)
		return err == nil && strconv.FormatUint(index, 10) == indexText
	default:
		return false
	}
}

func validateExportFile(metadata unix.Stat_t, uid, gid uint32, maximumBytes int64) error {
	if metadata.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("guest export file is not regular")
	}
	if metadata.Uid != uid || metadata.Gid != gid {
		return errors.New("guest export file owner does not match required identity")
	}
	if uint32(metadata.Mode)&0o7777 != 0o600 {
		return errors.New("guest export file mode must be 0600")
	}
	if metadata.Nlink != 1 {
		return errors.New("guest export file must have exactly one link")
	}
	if metadata.Size < 0 || metadata.Size > maximumBytes {
		return errors.New("guest export file exceeds size limit")
	}
	return nil
}

func verifyLinkedExportFile(parentFD int, leaf string, opened unix.Stat_t) error {
	var linked unix.Stat_t
	if err := unix.Fstatat(parentFD, leaf, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("revalidate guest export file: %w", err)
	}
	if linked.Mode&unix.S_IFMT != unix.S_IFREG || linked.Dev != opened.Dev || linked.Ino != opened.Ino || linked.Nlink != 1 {
		return errors.New("guest export file changed during transfer")
	}
	return nil
}

type nulRejectingWriter struct {
	writer io.Writer
}

func (writer nulRejectingWriter) Write(contents []byte) (int, error) {
	if bytes.IndexByte(contents, 0) >= 0 {
		return 0, errors.New("guest credential contains NUL")
	}
	return writer.writer.Write(contents)
}
