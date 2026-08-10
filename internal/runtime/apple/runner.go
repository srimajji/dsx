package apple

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Command struct {
	Executable string
	Args       []string
	Env        []string
	Dir        string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

type Result struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	ExitCode        int
	Signal          string
	Duration        time.Duration
}

type Runner interface {
	Run(context.Context, Command) (Result, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, command Command) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("runner context is nil")
	}
	if err := validateCommand(command); err != nil {
		return Result{}, err
	}

	process := exec.CommandContext(ctx, command.Executable, command.Args...)
	process.Env = make([]string, len(command.Env))
	copy(process.Env, command.Env)
	process.Dir = command.Dir
	process.Stdin = command.Stdin
	var stdout boundedBuffer
	var stderr boundedBuffer
	process.Stdout = captureAndStream(&stdout, command.Stdout)
	process.Stderr = captureAndStream(&stderr, command.Stderr)

	started := time.Now()
	err := process.Run()
	result := Result{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		ExitCode:        0,
		Duration:        time.Since(started),
	}
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.ExitCode = -1
		return result, fmt.Errorf("run %s: %w", command.Executable, ctxErr)
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.ExitCode = -1
			result.Signal = signalName(status.Signal())
		}
		return result, fmt.Errorf("run %s: %w", command.Executable, err)
	}
	result.ExitCode = -1
	return result, fmt.Errorf("start %s: %w", command.Executable, err)
}

const (
	maxCommandArguments    = 256
	maxCommandEnvironment  = 256
	maxCommandValueBytes   = 32 * 1024
	maxCapturedOutputBytes = 4 * 1024 * 1024
)

func validateCommand(command Command) error {
	if command.Executable == "" || !filepath.IsAbs(command.Executable) || filepath.Clean(command.Executable) != command.Executable {
		return fmt.Errorf("runner executable must be a clean absolute path, got %q", command.Executable)
	}
	if command.Dir != "" && (!filepath.IsAbs(command.Dir) || filepath.Clean(command.Dir) != command.Dir) {
		return fmt.Errorf("runner directory must be a clean absolute path, got %q", command.Dir)
	}
	if len(command.Args) > maxCommandArguments {
		return fmt.Errorf("runner argument count %d exceeds %d", len(command.Args), maxCommandArguments)
	}
	for index, argument := range command.Args {
		if len(argument) > maxCommandValueBytes || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("runner argument %d is invalid", index)
		}
	}
	if len(command.Env) > maxCommandEnvironment {
		return fmt.Errorf("runner environment count %d exceeds %d", len(command.Env), maxCommandEnvironment)
	}
	for index, entry := range command.Env {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 || len(entry) > maxCommandValueBytes || strings.IndexByte(entry, 0) >= 0 {
			return fmt.Errorf("runner environment entry %d is invalid", index)
		}
	}
	return nil
}

type boundedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := maxCapturedOutputBytes - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || originalLength != 0
		return originalLength, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		buffer.truncated = true
	}
	_, err := buffer.Buffer.Write(value)
	return originalLength, err
}

func (buffer *boundedBuffer) Truncated() bool { return buffer.truncated }

func captureAndStream(capture io.Writer, stream io.Writer) io.Writer {
	if stream == nil {
		return capture
	}
	return io.MultiWriter(capture, stream)
}

func signalName(signal syscall.Signal) string {
	name := strings.ToUpper(signal.String())
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + strings.ReplaceAll(name, " ", "")
	}
	return name
}
