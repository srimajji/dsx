package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const defaultRunnerOutputLimit = 16 << 20

// OSRunner executes Command.Argv directly. It never invokes a shell and bounds
// each output stream even when a caller supplies an unbounded writer.
type OSRunner struct {
	MaxOutputBytes int
}

func (runner OSRunner) Run(ctx context.Context, command Command) (Exit, error) {
	if ctx == nil {
		return Exit{Code: -1}, errors.New("git runner context is nil")
	}
	if err := validateRunnerCommand(command); err != nil {
		return Exit{Code: -1}, err
	}
	limit := runner.MaxOutputBytes
	if limit <= 0 {
		limit = defaultRunnerOutputLimit
	}
	runContext := ctx
	cancel := func() {}
	if command.StdoutMaxBytes > 0 {
		runContext, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	process := exec.CommandContext(runContext, command.Argv[0], command.Argv[1:]...)
	process.Dir = command.Dir
	process.Env = append([]string(nil), command.Env...)
	process.Stdin = command.Stdin
	process.Stdout = &boundedWriter{
		writer:    writerOrDiscard(command.Stdout),
		remaining: stdoutLimit(command, limit),
		failHard:  command.StdoutMaxBytes > 0,
		cancel:    cancel,
	}
	process.Stderr = &boundedWriter{writer: writerOrDiscard(command.Stderr), remaining: int64(limit)}

	err := process.Run()
	if err == nil {
		return Exit{Code: 0}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Exit{Code: -1}, fmt.Errorf("run %q: %w", command.Argv[0], ctxErr)
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exit := Exit{Code: exitError.ExitCode()}
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			exit.Code = -1
			exit.Signal = signalName(status.Signal())
		}
		return exit, fmt.Errorf("run %q: %w", command.Argv[0], err)
	}
	return Exit{Code: -1}, fmt.Errorf("start %q: %w", command.Argv[0], err)
}

func validateRunnerCommand(command Command) error {
	if len(command.Argv) == 0 || command.Argv[0] == "" {
		return errors.New("git runner argv is empty")
	}
	if len(command.Argv) > 512 {
		return errors.New("git runner argv exceeds 512 entries")
	}
	for index, argument := range command.Argv {
		if len(argument) > 32<<10 || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("git runner argv[%d] is invalid", index)
		}
	}
	if !filepath.IsAbs(command.Argv[0]) || filepath.Clean(command.Argv[0]) != command.Argv[0] {
		return fmt.Errorf("git runner executable must be a clean absolute path, got %q", command.Argv[0])
	}
	if command.Dir != "" && (!filepath.IsAbs(command.Dir) || filepath.Clean(command.Dir) != command.Dir) {
		return fmt.Errorf("git runner directory must be a clean absolute path, got %q", command.Dir)
	}
	if len(command.Env) > 512 {
		return errors.New("git runner environment exceeds 512 entries")
	}
	for index, entry := range command.Env {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 || len(entry) > 32<<10 || strings.IndexByte(entry, 0) >= 0 {
			return fmt.Errorf("git runner environment[%d] is invalid", index)
		}
	}
	if command.StdoutMaxBytes < 0 {
		return errors.New("git runner stdout limit is invalid")
	}
	return nil
}

type boundedWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	remaining int64
	failHard  bool
	cancel    context.CancelFunc
}

func (writer *boundedWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	original := len(value)
	if writer.remaining == 0 {
		if writer.failHard && original != 0 {
			writer.cancel()
			return 0, errOutputLimit
		}
		return original, nil
	}
	toWrite := value
	exceeded := int64(len(toWrite)) > writer.remaining
	if exceeded {
		toWrite = toWrite[:writer.remaining]
	}
	written, err := writer.writer.Write(toWrite)
	writer.remaining -= int64(written)
	if err != nil {
		if writer.failHard {
			writer.cancel()
		}
		return written, err
	}
	if written != len(toWrite) {
		if writer.failHard {
			writer.cancel()
		}
		return written, io.ErrShortWrite
	}
	if exceeded && writer.failHard {
		writer.cancel()
		return written, errOutputLimit
	}
	return original, nil
}

var errOutputLimit = errors.New("subprocess stdout exceeds limit")

func stdoutLimit(command Command, fallback int) int64 {
	if command.StdoutMaxBytes > 0 {
		return command.StdoutMaxBytes
	}
	return int64(fallback)
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

func signalName(signal syscall.Signal) string {
	name := strings.ToUpper(signal.String())
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + strings.ReplaceAll(name, " ", "")
	}
	return name
}
