package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/guest"
	"github.com/srimajji/dsx/internal/guestproto"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) == 0 {
		printUsage()
		return 2
	}
	switch arguments[0] {
	case "serve":
		return runServe(arguments[1:])
	case "ctl":
		return runControl(arguments[1:])
	case "ensure-dir":
		return runEnsureDirectory(arguments[1:])
	case "stage-file":
		return runStageFile(arguments[1:])
	case "produce-file":
		return runProduceFile(arguments[1:])
	case "export-file":
		return runExportFile(arguments[1:])
	case "remove-export-file":
		return runRemoveExportFile(arguments[1:])
	case "remove-read-only":
		return runRemoveReadOnly(arguments[1:])
	case "stage-env":
		return runStageEnvironment(arguments[1:])
	case "exec":
		return runExec(arguments[1:])
	case "relay-loopback":
		return runRelayLoopback(arguments[1:])
	default:
		return runVersion(arguments)
	}
}

func runVersion(arguments []string) int {
	flags := flag.NewFlagSet("dsx-guest", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print version")
	jsonOutput := flags.Bool("json", false, "print machine-readable output")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if !*showVersion || len(flags.Args()) != 0 {
		printUsage()
		return 2
	}
	info := buildinfo.Current()
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
			fmt.Fprintf(os.Stderr, "dsx-guest: write version: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stdout, "dsx-guest %s (commit %s, built %s, %s)\n", info.Version, info.Commit, info.BuiltAt, info.GoVersion)
	return 0
}

func runServe(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest serve: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socketPath := flags.String("socket", guest.DefaultSocketPath, "control socket path")
	childUIDText := flags.String("child-uid", "", "non-root workspace child UID")
	childGIDText := flags.String("child-gid", "", "workspace child GID")
	initializeWorkspace := flags.String("initialize-workspace", "", "owned clone workspace volume path")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest serve [--socket PATH] --child-uid UID --child-gid GID [--initialize-workspace /workspace]")
		return 2
	}
	childUID, childGID, err := parseChildIdentity(*childUIDText, *childGIDText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest serve: %v\n", err)
		fmt.Fprintln(os.Stderr, "usage: dsx-guest serve [--socket PATH] --child-uid UID --child-gid GID [--initialize-workspace /workspace]")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "dsx-guest serve: supervisor must start as root")
		return 1
	}
	if *initializeWorkspace != "" {
		if err := guest.InitializeOwnedWorkspace(*initializeWorkspace, childUID, childGID); err != nil {
			fmt.Fprintf(os.Stderr, "dsx-guest serve: initialize owned workspace: %v\n", err)
			return 1
		}
	}
	info := buildinfo.Current()
	supervisor, err := guest.NewSupervisor(guest.Options{Version: info.Version, ChildUID: childUID, ChildGID: childGID, Output: os.Stdout})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest serve: initialize supervisor: %v\n", err)
		return 1
	}
	server, err := guest.NewServer(supervisor, guest.ServerOptions{Path: *socketPath, ExpectedUID: &childUID, ExpectedGID: &childGID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest serve: initialize server: %v\n", err)
		return 1
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(60)*time.Second)
			defer cancel()
			_ = supervisor.Shutdown(ctx)
		case <-supervisor.Done():
		}
	}()
	if err := server.Serve(context.Background()); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(60)*time.Second)
		shutdownErr := supervisor.Shutdown(ctx)
		cancel()
		if shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "dsx-guest serve: shutdown after server failure: %v\n", shutdownErr)
		}
		fmt.Fprintf(os.Stderr, "dsx-guest serve: %v\n", err)
		return 1
	}
	<-supervisor.Done()
	return 0
}

func runControl(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest ctl: verify installed helper: %v\n", err)
		return 3
	}
	flags := flag.NewFlagSet("dsx-guest ctl", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socketPath := flags.String("socket", guest.DefaultSocketPath, "control socket path")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest ctl [--socket PATH]")
		return 2
	}
	ok, err := guest.Control(context.Background(), *socketPath, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest ctl: %v\n", err)
		if errors.Is(err, guest.ErrControlInput) {
			return 2
		}
		return 3
	}
	if !ok {
		return 1
	}
	return 0
}

func runEnsureDirectory(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest ensure-dir: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest ensure-dir", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	destination := flags.String("path", "", "authorized private run directory")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *destination == "" {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest ensure-dir --path PATH")
		return 2
	}
	if err := guest.EnsureRunDirectory(*destination); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest ensure-dir: %v\n", err)
		return 1
	}
	return 0
}

func runStageFile(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest stage-file: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest stage-file", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	readOnly := flags.Bool("read-only", false, "stage root-owned child-readable configuration")
	childUIDText := flags.String("child-uid", "", "non-root workspace child UID")
	childGIDText := flags.String("child-gid", "", "workspace child GID")
	destination := flags.String("path", "", "authorized private auth/config path")
	maxArtifactBytes := flags.Int64("max-bytes", 0, "maximum accepted artifact bytes")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *destination == "" || *maxArtifactBytes <= 0 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest stage-file [--read-only --child-uid UID --child-gid GID] --max-bytes BYTES --path PATH")
		return 2
	}
	var err error
	if *readOnly {
		var childUID, childGID uint32
		childUID, childGID, err = parseChildIdentity(*childUIDText, *childGIDText)
		if err == nil {
			err = guest.StageReadOnlyRunFile(*destination, os.Stdin, *maxArtifactBytes, childUID, childGID)
		}
	} else if *childUIDText != "" || *childGIDText != "" {
		err = errors.New("child identity is valid only with --read-only")
	} else {
		err = guest.StageRunFile(*destination, os.Stdin, *maxArtifactBytes)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest stage-file: %v\n", err)
		return 1
	}
	return 0
}

func runProduceFile(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest produce-file: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest produce-file", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	destination := flags.String("path", "", "authorized private result path")
	workingDirectory := flags.String("cwd", "", "producer working directory")
	maximumBytes := flags.Int64("max-bytes", 0, "maximum produced bytes")
	if err := flags.Parse(arguments); err != nil || *destination == "" || *workingDirectory == "" || *maximumBytes <= 0 || len(flags.Args()) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest produce-file --max-bytes BYTES --path PATH --cwd PATH -- COMMAND [ARG...]")
		return 2
	}
	command := guestproto.CommandSpec{Argv: append([]string(nil), flags.Args()...), Cwd: *workingDirectory}
	if err := guest.ProduceRunFile(context.Background(), *destination, *maximumBytes, command); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest produce-file: %v\n", err)
		return 1
	}
	return 0
}

func runExportFile(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest export-file: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest export-file", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	source := flags.String("path", "", "authorized private run file")
	kind := flags.String("kind", "", "export kind (auth or result)")
	maximumBytes := flags.Int64("max-bytes", 0, "maximum exported bytes")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *source == "" || (*kind != string(guest.ExportAuth) && *kind != string(guest.ExportResult)) || *maximumBytes <= 0 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest export-file --kind auth|result --max-bytes BYTES --path PATH")
		return 2
	}
	if err := guest.ExportRunFile(*source, guest.ExportKind(*kind), *maximumBytes, os.Stdout); err != nil {
		if errors.Is(err, guest.ErrRunArtifactMissing) {
			return 4
		}
		fmt.Fprintf(os.Stderr, "dsx-guest export-file: %v\n", err)
		return 1
	}
	return 0
}

func runRemoveExportFile(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest remove-export-file: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest remove-export-file", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	source := flags.String("path", "", "authorized result export file")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *source == "" {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest remove-export-file --path PATH")
		return 2
	}
	if err := guest.RemoveRunExportFile(*source); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest remove-export-file: %v\n", err)
		return 1
	}
	return 0
}

func runRemoveReadOnly(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest remove-read-only: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest remove-read-only", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	destination := flags.String("path", "", "authorized read-only run root")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *destination == "" {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest remove-read-only --path PATH")
		return 2
	}
	if err := guest.RemoveReadOnlyRunRoot(*destination); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest remove-read-only: %v\n", err)
		return 1
	}
	return 0
}

func runStageEnvironment(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest stage-env: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest stage-env", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	destination := flags.String("path", "", "authorized secret environment path")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *destination == "" {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest stage-env --path PATH")
		return 2
	}
	if err := guest.StageSecretEnvironment(*destination, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest stage-env: %v\n", err)
		return 1
	}
	return 0
}

func runExec(arguments []string) int {
	environmentFile, argv, err := parseExecArguments(arguments)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest exec [--env-file PATH] -- COMMAND [ARG...]")
		return 2
	}
	if err := guest.ReplaceProcessWithoutPrivilegeGains(argv, environmentFile); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest exec: %v\n", err)
		return 126
	}
	return 0
}

func runRelayLoopback(arguments []string) int {
	if err := guest.VerifyInstalledExecutable(); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest relay-loopback: verify installed helper: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("dsx-guest relay-loopback", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	port := flags.Uint("port", 0, "guest loopback TCP port")
	if err := flags.Parse(arguments); err != nil || len(flags.Args()) != 0 || *port == 0 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "usage: dsx-guest relay-loopback --port PORT")
		return 2
	}
	if err := guest.RelayLoopback(os.Stdin, os.Stdout, uint16(*port)); err != nil {
		fmt.Fprintf(os.Stderr, "dsx-guest relay-loopback: %v\n", err)
		return 1
	}
	return 0
}

func parseExecArguments(arguments []string) (string, []string, error) {
	environmentFile := ""
	hasEnvironmentFile := len(arguments) > 0 && arguments[0] == "--env-file"
	if hasEnvironmentFile {
		if len(arguments) < 2 {
			return "", nil, errors.New("invalid exec arguments")
		}
		environmentFile = arguments[1]
		arguments = arguments[2:]
	}
	if len(arguments) < 2 || arguments[0] != "--" || arguments[1] == "" || hasEnvironmentFile && environmentFile == "" {
		return "", nil, errors.New("invalid exec arguments")
	}
	return environmentFile, arguments[1:], nil
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: dsx-guest --version [--json] | dsx-guest serve [--socket PATH] --child-uid UID --child-gid GID [--initialize-workspace /workspace] | dsx-guest ctl [--socket PATH] | dsx-guest ensure-dir --path PATH | dsx-guest stage-file [--read-only --child-uid UID --child-gid GID] --max-bytes BYTES --path PATH | dsx-guest produce-file --max-bytes BYTES --path PATH --cwd PATH -- COMMAND [ARG...] | dsx-guest export-file --kind auth|result --max-bytes BYTES --path PATH | dsx-guest remove-export-file --path PATH | dsx-guest remove-read-only --path PATH | dsx-guest stage-env --path PATH | dsx-guest relay-loopback --port PORT | dsx-guest exec [--env-file PATH] -- COMMAND [ARG...]")
}

func parseChildIdentity(uidText, gidText string) (uint32, uint32, error) {
	uid, err := parseUint32(uidText)
	if err != nil || uid == 0 {
		return 0, 0, errors.New("child-uid must be a non-zero uint32")
	}
	gid, err := parseUint32(gidText)
	if err != nil || gid == 0 {
		return 0, 0, errors.New("child-gid must be a non-zero uint32")
	}
	return uid, gid, nil
}

func parseUint32(value string) (uint32, error) {
	if value == "" {
		return 0, errors.New("value is empty")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("value is not unsigned decimal")
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	return uint32(parsed), err
}
