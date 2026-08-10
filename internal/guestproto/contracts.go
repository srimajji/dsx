package guestproto

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/srimajji/dsx/internal/model"
)

const (
	MaxProcesses        = 128
	MaxSetupCommands    = 64
	MaxCommandArgs      = 256
	MaxEnvironment      = 256
	MaxDependencies     = 64
	MaxLogBytes         = 1 << 20
	MaxStringBytes      = 4096
	MaxHealthRetries    = 1000
	MaxHealthIntervalMS = 60_000
)

type ProcessState string

const (
	StateConfigured          ProcessState = "configured"
	StateWaitingDependencies ProcessState = "waiting_dependencies"
	StateStarting            ProcessState = "starting"
	StateRunning             ProcessState = "running"
	StateReady               ProcessState = "ready"
	StateStopping            ProcessState = "stopping"
	StateExited              ProcessState = "exited"
	StateUnhealthy           ProcessState = "unhealthy"
	StateFailed              ProcessState = "failed"
)

type CommandSpec struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env,omitempty"`
}

type HealthSpec struct {
	Kind       string       `json:"kind"`
	Target     string       `json:"target,omitempty"`
	Command    *CommandSpec `json:"command,omitempty"`
	IntervalMS int64        `json:"interval_ms"`
	TimeoutMS  int64        `json:"timeout_ms"`
	Retries    int          `json:"retries"`
}

type ProcessSpec struct {
	ID        string      `json:"id"`
	Command   CommandSpec `json:"command"`
	DependsOn []string    `json:"depends_on,omitempty"`
	Required  bool        `json:"required"`
	Terminal  bool        `json:"terminal"`
	Health    *HealthSpec `json:"health,omitempty"`
}

type StartParams struct {
	Setup         []CommandSpec `json:"setup,omitempty"`
	Processes     []ProcessSpec `json:"processes"`
	LogLimitBytes int           `json:"log_limit_bytes"`
}

type SignalParams struct {
	Signal string `json:"signal"`
}

type ResizeParams struct {
	Columns uint16 `json:"columns"`
	Rows    uint16 `json:"rows"`
}

type ExitStatus struct {
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type ProcessStatus struct {
	ID         string       `json:"id"`
	Generation uint64       `json:"generation"`
	State      ProcessState `json:"state"`
	Ready      bool         `json:"ready"`
	Required   bool         `json:"required"`
	Exit       *ExitStatus  `json:"exit,omitempty"`
	Failure    string       `json:"failure,omitempty"`
	Log        string       `json:"log,omitempty"`
	LogDropped uint64       `json:"log_dropped_bytes,omitempty"`
}

type StatusResult struct {
	Generation uint64          `json:"generation"`
	Failed     bool            `json:"failed"`
	Processes  []ProcessStatus `json:"processes"`
}

type StartResult struct {
	Generation uint64 `json:"generation"`
}

func (params StartParams) Validate() error {
	if len(params.Setup) > MaxSetupCommands {
		return fmt.Errorf("setup command count exceeds %d", MaxSetupCommands)
	}
	if len(params.Processes) > MaxProcesses || (len(params.Processes) == 0 && len(params.Setup) == 0) {
		return fmt.Errorf("start graph must contain setup or 1..%d processes", MaxProcesses)
	}
	if params.LogLimitBytes < 1 || params.LogLimitBytes > MaxLogBytes {
		return fmt.Errorf("log_limit_bytes must be within 1..%d", MaxLogBytes)
	}
	for index := range params.Setup {
		if err := params.Setup[index].Validate(); err != nil {
			return fmt.Errorf("setup[%d]: %w", index, err)
		}
	}
	seen := make(map[string]struct{}, len(params.Processes))
	for index := range params.Processes {
		process := &params.Processes[index]
		parsed, err := model.ParseSandboxName(process.ID)
		if err != nil || string(parsed) != process.ID {
			return fmt.Errorf("process[%d] has invalid id %q", index, process.ID)
		}
		if _, duplicate := seen[process.ID]; duplicate {
			return fmt.Errorf("duplicate process id %q", process.ID)
		}
		seen[process.ID] = struct{}{}
		if err := process.Command.Validate(); err != nil {
			return fmt.Errorf("process %q: %w", process.ID, err)
		}
		if len(process.DependsOn) > MaxDependencies {
			return fmt.Errorf("process %q has too many dependencies", process.ID)
		}
		if process.Health != nil {
			if err := process.Health.Validate(); err != nil {
				return fmt.Errorf("process %q health: %w", process.ID, err)
			}
		}
	}
	for _, process := range params.Processes {
		for _, dependency := range process.DependsOn {
			if dependency == process.ID {
				return fmt.Errorf("process %q depends on itself", process.ID)
			}
			if _, found := seen[dependency]; !found {
				return fmt.Errorf("process %q depends on unknown process %q", process.ID, dependency)
			}
		}
	}
	visiting := make(map[string]bool, len(seen))
	visited := make(map[string]bool, len(seen))
	byID := make(map[string]ProcessSpec, len(params.Processes))
	for _, process := range params.Processes {
		byID[process.ID] = process
	}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("process dependency cycle includes %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (command CommandSpec) Validate() error {
	if len(command.Argv) == 0 || len(command.Argv) > MaxCommandArgs || command.Argv[0] == "" {
		return fmt.Errorf("argv must contain 1..%d non-empty executable arguments", MaxCommandArgs)
	}
	if !strings.HasPrefix(command.Cwd, "/") || len(command.Cwd) > MaxStringBytes || strings.ContainsRune(command.Cwd, '\x00') {
		return errors.New("cwd must be a bounded absolute guest path")
	}
	for _, argument := range command.Argv {
		if argument == "" || len(argument) > MaxStringBytes || strings.ContainsRune(argument, '\x00') {
			return errors.New("argv contains an empty, oversized, or NUL argument")
		}
	}
	if len(command.Env) > MaxEnvironment {
		return fmt.Errorf("environment exceeds %d entries", MaxEnvironment)
	}
	seen := make(map[string]struct{}, len(command.Env))
	for _, entry := range command.Env {
		if len(entry) > MaxStringBytes || strings.ContainsRune(entry, '\x00') {
			return errors.New("environment contains an oversized or NUL entry")
		}
		name, _, found := strings.Cut(entry, "=")
		if !found || !validEnvName(name) {
			return fmt.Errorf("invalid environment entry %q", entry)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate environment name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (health HealthSpec) Validate() error {
	if health.IntervalMS < 1 || health.IntervalMS > MaxHealthIntervalMS || health.TimeoutMS < 1 || health.TimeoutMS > MaxHealthIntervalMS || health.Retries < 1 || health.Retries > MaxHealthRetries {
		return errors.New("health timing is out of bounds")
	}
	switch health.Kind {
	case "http":
		parsed, err := url.Parse(health.Target)
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || health.Command != nil {
			return errors.New("HTTP health requires a plain http URL and no command")
		}
	case "tcp":
		if _, err := netip.ParseAddrPort(health.Target); err != nil || health.Command != nil {
			return errors.New("TCP health requires an IP:port target and no command")
		}
	case "command":
		if health.Command == nil || health.Target != "" {
			return errors.New("command health requires only a command")
		}
		if err := health.Command.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported health kind %q", health.Kind)
	}
	return nil
}

func (state ProcessState) Valid() bool {
	switch state {
	case StateConfigured, StateWaitingDependencies, StateStarting, StateRunning, StateReady, StateUnhealthy, StateStopping, StateExited, StateFailed:
		return true
	default:
		return false
	}
}

func validEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}
