package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/hostsource"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
)

const (
	maxItems       = 128
	maxCPUs        = 64
	minMemoryBytes = 128 << 20
	maxMemoryBytes = 1 << 40
)

var nameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
var volumeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var labelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*(?:@[A-Za-z0-9]+:[0-9a-f]+)?$`)

type OperationError struct {
	Operation string
	ExitCode  int
	Signal    string
	Stderr    string
	Cause     error
}

func (e *OperationError) Error() string {
	message := fmt.Sprintf("Apple runtime %s failed (exit %d, signal %q)", e.Operation, e.ExitCode, e.Signal)
	if e.Stderr != "" {
		message += ": " + e.Stderr
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}
func (e *OperationError) Unwrap() error      { return e.Cause }
func bad(op string, err error) error         { return model.Wrap(model.CodeInvalidInput, op, err) }
func unavailable(op string, err error) error { return model.Wrap(model.CodeUnavailable, op, err) }

func (a *Adapter) ready(ctx context.Context) error {
	if a == nil || a.runner == nil || a.containerExecutable == "" {
		return bad("Apple adapter", errors.New("adapter is not configured"))
	}
	if ctx == nil {
		return bad("Apple adapter", errors.New("context is nil"))
	}
	return nil
}
func (a *Adapter) command(ctx context.Context, op string, c Command) (Result, error) {
	c.Executable = a.containerExecutable
	c.Env = append([]string(nil), probeEnvironment...)
	r, e := a.runner.Run(ctx, c)
	if e == nil && r.ExitCode == 0 && r.Signal == "" {
		return r, nil
	}
	cause := e
	if ce := ctx.Err(); ce != nil {
		cause = ce
	}
	stderr := strings.TrimSpace(string(r.Stderr))
	if len(stderr) > 4096 {
		stderr = stderr[:4096]
	}
	return r, unavailable(op, &OperationError{Operation: op, ExitCode: r.ExitCode, Signal: r.Signal, Stderr: stderr, Cause: cause})
}
func missing(err error, noun, name string) bool {
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.ExitCode != 1 || operationError.Signal != "" {
		return false
	}
	return operationError.Stderr == fmt.Sprintf("Error: %s not found: %s", noun, name)
}

func (a *Adapter) EnsureImage(ctx context.Context, s runtime.ImageSpec) (runtime.Image, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.Image{}, e
	}
	build := s.Context != "" || s.File != ""
	if build {
		if e := validBuild(s); e != nil {
			return runtime.Image{}, bad("build image", e)
		}
		if s.Reuse {
			observed, inspectErr := a.image(ctx, s.Reference)
			if inspectErr == nil && observed.hasLabels(s.Labels) {
				return runtime.Image{Reference: observed.reference, Digest: observed.descriptorDigest, Local: true}, nil
			}
			if inspectErr != nil && !missing(inspectErr, "image", s.Reference) {
				return runtime.Image{}, inspectErr
			}
		}
		args := []string{"build", "--progress", "plain", "--tag", s.Reference, "--file", string(s.File)}
		if s.Target != "" {
			args = append(args, "--target", s.Target)
		}
		for _, v := range s.BuildArgs {
			args = append(args, "--build-arg", v.Key+"="+v.Value)
		}
		args = labelArgs(args, s.Labels)
		args = append(args, string(s.Context))
		if _, e := a.command(ctx, "build image", Command{Args: args}); e != nil {
			return runtime.Image{}, e
		}
		observed, e := a.image(ctx, s.Reference)
		if e != nil {
			return runtime.Image{}, e
		}
		if s.Reuse && !observed.hasLabels(s.Labels) {
			return runtime.Image{}, unavailable("verify built image", errors.New("managed image labels do not match the approved build input"))
		}
		return runtime.Image{Reference: observed.reference, Digest: observed.descriptorDigest, Local: true}, nil
	}

	expected, e := pin(s.Reference)
	if e != nil {
		return runtime.Image{}, bad("ensure image", e)
	}
	observed, inspectErr := a.image(ctx, s.Reference)
	if inspectErr == nil {
		if !observed.hasDigest(expected) {
			return runtime.Image{}, unavailable("verify image", fmt.Errorf("pinned digest %q is absent from inspected reference, descriptor, and variants", expected))
		}
		return runtime.Image{Reference: s.Reference, Digest: expected}, nil
	}
	if !missing(inspectErr, "image", s.Reference) {
		return runtime.Image{}, inspectErr
	}

	localReference := s.Reference[:strings.LastIndex(s.Reference, "@sha256:")]
	if localReference != "" {
		local, localErr := a.image(ctx, localReference)
		if localErr == nil && local.hasDigest(expected) {
			return runtime.Image{Reference: localReference, Digest: expected, Local: true}, nil
		}
		if localErr != nil && !missing(localErr, "image", localReference) {
			return runtime.Image{}, localErr
		}
	}
	if _, e := a.command(ctx, "pull image", Command{Args: []string{"image", "pull", "--progress", "plain", s.Reference}}); e != nil {
		return runtime.Image{}, e
	}
	observed, e = a.image(ctx, s.Reference)
	if e != nil {
		return runtime.Image{}, e
	}
	if !observed.hasDigest(expected) {
		return runtime.Image{}, unavailable("verify image", fmt.Errorf("pinned digest %q is absent from inspected reference, descriptor, and variants", expected))
	}
	return runtime.Image{Reference: s.Reference, Digest: expected}, nil
}

type inspectedImage struct {
	reference        string
	descriptorDigest string
	variantDigests   []string
	variantLabels    []map[string]string
}

func (image inspectedImage) hasDigest(expected string) bool {
	if image.descriptorDigest == expected {
		return true
	}
	if referenceDigest, err := pin(image.reference); err == nil && referenceDigest == expected {
		return true
	}
	for _, digest := range image.variantDigests {
		if digest == expected {
			return true
		}
	}
	return false
}

func (image inspectedImage) hasLabels(expected []runtime.Label) bool {
	if len(expected) == 0 {
		return true
	}
	for _, labels := range image.variantLabels {
		matches := true
		for _, label := range expected {
			if labels[label.Key] != label.Value {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func (a *Adapter) image(ctx context.Context, ref string) (inspectedImage, error) {
	r, e := a.command(ctx, "inspect image", Command{Args: []string{"image", "inspect", ref}})
	if e != nil {
		return inspectedImage{}, e
	}
	var x []struct {
		Configuration struct {
			Name       string `json:"name"`
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"configuration"`
		Variants []struct {
			Digest string `json:"digest"`
			Config struct {
				Config struct {
					Labels map[string]string `json:"Labels"`
				} `json:"config"`
			} `json:"config"`
		} `json:"variants"`
	}
	if e = decodeJSON(r.Stdout, &x, false); e != nil {
		return inspectedImage{}, unavailable("decode image", e)
	}
	if len(x) != 1 || x[0].Configuration.Name == "" || !digestRE.MatchString(x[0].Configuration.Descriptor.Digest) {
		return inspectedImage{}, unavailable("decode image", errors.New("expected one image and sha256 descriptor digest"))
	}
	observed := inspectedImage{
		reference:        x[0].Configuration.Name,
		descriptorDigest: x[0].Configuration.Descriptor.Digest,
		variantDigests:   make([]string, 0, len(x[0].Variants)),
		variantLabels:    make([]map[string]string, 0, len(x[0].Variants)),
	}
	for _, variant := range x[0].Variants {
		if !digestRE.MatchString(variant.Digest) {
			return inspectedImage{}, unavailable("decode image", fmt.Errorf("invalid variant digest %q", variant.Digest))
		}
		observed.variantDigests = append(observed.variantDigests, variant.Digest)
		observed.variantLabels = append(observed.variantLabels, variant.Config.Config.Labels)
	}
	return observed, nil
}

func (a *Adapter) CreateVolume(ctx context.Context, s runtime.VolumeSpec) (runtime.Resource, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.Resource{}, e
	}
	if !volumeRE.MatchString(s.Name) || len(s.Name) > 63 {
		return runtime.Resource{}, bad("create volume", errors.New("invalid name"))
	}
	if e := own(s.Name, runtime.ResourceVolume, s.Labels); e != nil {
		return runtime.Resource{}, bad("create volume", e)
	}
	args := labelArgs([]string{"volume", "create"}, s.Labels)
	args = append(args, s.Name)
	r, e := a.command(ctx, "create volume", Command{Args: args})
	if e != nil {
		return runtime.Resource{}, e
	}
	snapshot, e := a.afterCreate(ctx, r, s.Name, runtime.ResourceVolume, s.Labels)
	return snapshot.Resource, e
}
func (a *Adapter) CreateNetwork(ctx context.Context, s runtime.NetworkSpec) (runtime.Resource, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.Resource{}, e
	}
	if !nameRE.MatchString(s.Name) {
		return runtime.Resource{}, bad("create network", errors.New("invalid name"))
	}
	if e := own(s.Name, runtime.ResourceNetwork, s.Labels); e != nil {
		return runtime.Resource{}, bad("create network", e)
	}
	args := labelArgs([]string{"network", "create"}, s.Labels)
	args = append(args, s.Name)
	r, e := a.command(ctx, "create network", Command{Args: args})
	if e != nil {
		return runtime.Resource{}, e
	}
	snapshot, e := a.afterCreate(ctx, r, s.Name, runtime.ResourceNetwork, s.Labels)
	return snapshot.Resource, e
}
func (a *Adapter) afterCreate(ctx context.Context, result Result, name string, kind runtime.ResourceKind, expectedLabels []runtime.Label) (runtime.ResourceSnapshot, error) {
	if result.StdoutTruncated || string(result.Stdout) != name && string(result.Stdout) != name+"\n" {
		return runtime.ResourceSnapshot{}, unavailable("verify create output", fmt.Errorf("returned %q", strings.TrimSpace(string(result.Stdout))))
	}
	snapshot, err := a.inspectKind(ctx, kind, runtime.ResourceID(name))
	if err != nil {
		return runtime.ResourceSnapshot{}, err
	}
	if snapshot.ID != runtime.ResourceID(name) || snapshot.Name != name || snapshot.Kind != kind || !sameLabels(snapshot.Labels, expectedLabels) {
		return runtime.ResourceSnapshot{}, unavailable("verify created resource", errors.New("identity, kind, or labels mismatch"))
	}
	return snapshot, nil
}

func (a *Adapter) CreateWorkspace(ctx context.Context, s runtime.WorkspaceSpec) (runtime.Resource, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.Resource{}, e
	}
	kind, e := validWorkspace(s)
	if e != nil {
		return runtime.Resource{}, bad("create workspace", e)
	}
	args := []string{"create", "--name", s.Name, "--user", s.User, "--workdir", string(s.WorkingDir)}
	for _, v := range s.Env {
		args = append(args, "--env", v)
	}
	for _, m := range s.Mounts {
		v, e := mountArg(m)
		if e != nil {
			return runtime.Resource{}, bad("create workspace", e)
		}
		args = append(args, "--mount", v)
	}
	for _, n := range s.Networks {
		args = append(args, "--network", n)
	}
	for _, p := range s.Ports {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1:%d:%d/%s", *p.HostPort, p.GuestPort, p.Protocol))
	}
	args = labelArgs(args, s.Labels)
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.MemoryBytes, 10))
	}
	args = append(args, "--entrypoint", s.Entrypoint[0], imageRef(s.Image))
	args = append(args, s.Entrypoint[1:]...)
	if s.Image.Local {
		observed, inspectErr := a.image(ctx, s.Image.Reference)
		if inspectErr != nil {
			return runtime.Resource{}, inspectErr
		}
		if !observed.hasDigest(s.Image.Digest) {
			return runtime.Resource{}, unavailable("verify local image before create", fmt.Errorf("expected digest %q is absent from inspected image %q", s.Image.Digest, s.Image.Reference))
		}
	}
	r, e := a.command(ctx, "create workspace", Command{Args: args})
	if e != nil {
		return runtime.Resource{}, e
	}
	snapshot, e := a.afterCreate(ctx, r, s.Name, kind, s.Labels)
	if e != nil {
		return runtime.Resource{}, e
	}
	if s.Image.Local && snapshot.ImageDigest != s.Image.Digest {
		verifyErr := unavailable("verify created workspace", fmt.Errorf("local image digest mismatch: expected %q, observed %q", s.Image.Digest, snapshot.ImageDigest))
		return runtime.Resource{}, errors.Join(verifyErr, a.Delete(ctx, snapshot))
	}
	if e := runtime.VerifyWorkspacePostcondition(snapshot, s); e != nil {
		return runtime.Resource{}, unavailable("verify created workspace", e)
	}
	return snapshot.Resource, nil
}
func (a *Adapter) CreateBrowser(ctx context.Context, s runtime.BrowserSpec) (runtime.Resource, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.Resource{}, e
	}
	if e := validBrowser(s); e != nil {
		return runtime.Resource{}, bad("create browser", e)
	}
	args := []string{"create", "--name", s.Name}
	for _, value := range s.Env {
		args = append(args, "--env", value)
	}
	args = append(args, "--network", s.Networks[0])
	args = labelArgs(args, s.Labels)
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.MemoryBytes, 10))
	}
	args = append(args, "--entrypoint", s.Entrypoint[0], imageRef(s.Image))
	args = append(args, s.Entrypoint[1:]...)
	if s.Image.Local {
		observed, inspectErr := a.image(ctx, s.Image.Reference)
		if inspectErr != nil {
			return runtime.Resource{}, inspectErr
		}
		if !observed.hasDigest(s.Image.Digest) {
			return runtime.Resource{}, unavailable("verify local image before create", fmt.Errorf("expected digest %q is absent from inspected image %q", s.Image.Digest, s.Image.Reference))
		}
	}
	result, e := a.command(ctx, "create browser", Command{Args: args})
	if e != nil {
		return runtime.Resource{}, e
	}
	snapshot, e := a.afterCreate(ctx, result, s.Name, runtime.ResourceBrowser, s.Labels)
	if e != nil {
		return runtime.Resource{}, e
	}
	if e := verifyBrowserPostcondition(snapshot, s); e != nil {
		return runtime.Resource{}, unavailable("verify created browser", e)
	}
	return snapshot.Resource, nil
}

func (a *Adapter) StartWorkspace(ctx context.Context, expected runtime.ResourceSnapshot) error {
	observed, e := a.verifyExpected(ctx, expected, "start")
	if e != nil {
		return e
	}
	if _, e = a.command(ctx, "start", Command{Args: []string{"start", string(observed.ID)}}); e != nil {
		return e
	}
	started, e := a.container(ctx, observed.ID)
	if e != nil {
		return e
	}
	if started.State != "running" || started.Resource != observed.Resource || !sameLabels(started.Labels, expected.Labels) {
		return unavailable("verify start", fmt.Errorf("state or ownership changed after start"))
	}
	if started.Kind == runtime.ResourceBrowser {
		if e := verifyBrowserNetworkAddress(started); e != nil {
			return unavailable("verify started browser", e)
		}
	}
	return nil
}
func (a *Adapter) PrepareExec(ctx context.Context, expected runtime.ResourceSnapshot, s runtime.ExecSpec) (runtime.ProcessSpec, error) {
	observed, e := a.verifyExpected(ctx, expected, "prepare exec")
	if e != nil {
		return runtime.ProcessSpec{}, e
	}
	if e := validExec(s); e != nil {
		return runtime.ProcessSpec{}, bad("prepare exec", e)
	}
	args := []string{"exec"}
	if s.Terminal {
		args = append(args, "--interactive", "--tty")
	}
	for _, value := range s.Env {
		args = append(args, "--env", value)
	}
	if s.WorkingDir != "" {
		args = append(args, "--workdir", string(s.WorkingDir))
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	args = append(args, string(observed.ID))
	args = append(args, s.Argv...)
	return runtime.ProcessSpec{Executable: a.containerExecutable, Args: args, Env: append([]string(nil), probeEnvironment...)}, nil
}

func (a *Adapter) Exec(ctx context.Context, expected runtime.ResourceSnapshot, s runtime.ExecSpec, io runtime.ExecIO) (runtime.Exit, error) {
	process, e := a.PrepareExec(ctx, expected, s)
	if e != nil {
		return runtime.Exit{}, e
	}
	if io.Stdin != nil && !s.Terminal {
		process.Args = append(process.Args[:1], append([]string{"--interactive"}, process.Args[1:]...)...)
	}
	r, e := a.runner.Run(ctx, Command{Executable: process.Executable, Args: process.Args, Env: process.Env, Dir: process.Dir, Stdin: io.Stdin, Stdout: io.Stdout, Stderr: io.Stderr})
	if e == nil {
		code := r.ExitCode
		return runtime.Exit{Code: &code}, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return runtime.Exit{}, unavailable("exec", contextErr)
	}
	if r.Signal != "" {
		return runtime.Exit{Signal: r.Signal}, nil
	}
	if r.ExitCode >= 0 {
		code := r.ExitCode
		return runtime.Exit{Code: &code}, nil
	}
	return runtime.Exit{}, unavailable("exec", e)
}
func (a *Adapter) CopyTo(ctx context.Context, expected runtime.ResourceSnapshot, h runtime.HostPath, g runtime.GuestPath) error {
	observed, e := a.verifyExpected(ctx, expected, "copy to")
	if e != nil {
		return e
	}
	if e := hostPath(h); e != nil {
		return bad("copy to", e)
	}
	if e := guestPath(g); e != nil {
		return bad("copy to", e)
	}
	_, e = a.command(ctx, "copy to", Command{Args: []string{"copy", string(h), string(observed.ID) + ":" + string(g)}})
	return e
}
func (a *Adapter) CopyFrom(ctx context.Context, expected runtime.ResourceSnapshot, g runtime.GuestPath, h runtime.HostPath) error {
	observed, e := a.verifyExpected(ctx, expected, "copy from")
	if e != nil {
		return e
	}
	if e := guestPath(g); e != nil {
		return bad("copy from", e)
	}
	if e := hostPath(h); e != nil {
		return bad("copy from", e)
	}
	_, e = a.command(ctx, "copy from", Command{Args: []string{"copy", string(observed.ID) + ":" + string(g), string(h)}})
	return e
}
func (a *Adapter) Inspect(ctx context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.ResourceSnapshot{}, e
	}
	if e := validID(id); e != nil {
		return runtime.ResourceSnapshot{}, bad("inspect", e)
	}
	s, e := a.container(ctx, id)
	if e == nil {
		return s, nil
	}
	if !missing(e, "container", string(id)) {
		return s, e
	}
	for _, k := range []runtime.ResourceKind{runtime.ResourceNetwork, runtime.ResourceVolume} {
		s, e = a.inspectKind(ctx, k, id)
		if e == nil {
			return s, nil
		}
		if !missing(e, resourceNoun(k), string(id)) {
			return s, e
		}
	}
	return s, unavailable("inspect", fmt.Errorf("%w: %q", runtime.ErrResourceNotFound, id))
}
func (a *Adapter) List(ctx context.Context, k runtime.ResourceKind) ([]runtime.ResourceSnapshot, error) {
	if e := a.ready(ctx); e != nil {
		return nil, e
	}
	var args []string
	switch k {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser:
		args = []string{"list", "--all", "--format", "json"}
	case runtime.ResourceNetwork:
		args = []string{"network", "list", "--format", "json"}
	case runtime.ResourceVolume:
		args = []string{"volume", "list", "--format", "json"}
	default:
		return nil, bad("list", errors.New("unknown kind"))
	}
	r, e := a.command(ctx, "list", Command{Args: args})
	if e != nil {
		return nil, e
	}
	var out []runtime.ResourceSnapshot
	if k == runtime.ResourceWorkspace || k == runtime.ResourceBrowser {
		out, e = decodeContainers(r.Stdout)
		if e == nil {
			filtered := out[:0]
			for _, s := range out {
				if s.Kind == k {
					filtered = append(filtered, s)
				}
			}
			out = filtered
		}
	} else {
		out, e = decodeNamed(r.Stdout, k)
	}
	if e != nil {
		return nil, unavailable("decode list", e)
	}
	return out, nil
}
func (a *Adapter) Signal(ctx context.Context, expected runtime.ResourceSnapshot, requested runtime.Signal) error {
	observed, e := a.verifyExpected(ctx, expected, "signal")
	if e != nil {
		return e
	}
	value, e := signal(requested)
	if e != nil {
		return bad("signal", e)
	}
	_, e = a.command(ctx, "signal", Command{Args: []string{"kill", "--signal", value, string(observed.ID)}})
	return e
}
func (a *Adapter) Stop(ctx context.Context, expected runtime.ResourceSnapshot, policy runtime.StopPolicy) error {
	observed, e := a.verifyExpected(ctx, expected, "stop")
	if e != nil {
		return e
	}
	if policy.TimeoutSeconds < 0 || policy.TimeoutSeconds > 300 {
		return bad("stop", errors.New("timeout out of range"))
	}
	args := []string{"stop"}
	if policy.Signal != "" {
		value, signalErr := signal(policy.Signal)
		if signalErr != nil {
			return bad("stop", signalErr)
		}
		args = append(args, "--signal", value)
	}
	args = append(args, "--time", strconv.Itoa(policy.TimeoutSeconds), string(observed.ID))
	if _, e := a.command(ctx, "stop", Command{Args: args}); e != nil {
		return e
	}
	stopped, e := a.container(ctx, observed.ID)
	if e != nil {
		return e
	}
	if stopped.State != "stopped" || stopped.Resource != observed.Resource || !sameLabels(stopped.Labels, expected.Labels) {
		return unavailable("verify stop", fmt.Errorf("state or ownership changed after stop"))
	}
	return nil
}

func (a *Adapter) verifyExpected(ctx context.Context, expected runtime.ResourceSnapshot, operation string) (runtime.ResourceSnapshot, error) {
	if e := a.ready(ctx); e != nil {
		return runtime.ResourceSnapshot{}, e
	}
	if e := validDelete(expected.Resource); e != nil {
		return runtime.ResourceSnapshot{}, bad(operation, e)
	}
	if len(expected.Labels) == 0 {
		return runtime.ResourceSnapshot{}, bad(operation, errors.New("expected ownership labels are missing"))
	}
	observed, e := a.inspectKind(ctx, expected.Kind, expected.ID)
	if e != nil {
		return runtime.ResourceSnapshot{}, e
	}
	if observed.Resource != expected.Resource || !sameLabels(observed.Labels, expected.Labels) ||
		!reflect.DeepEqual(observed.Mounts, expected.Mounts) ||
		!reflect.DeepEqual(observed.Networks, expected.Networks) ||
		!reflect.DeepEqual(observed.Ports, expected.Ports) {
		return runtime.ResourceSnapshot{}, bad(operation, errors.New("typed resource, ownership labels, or runtime grants do not match inspection"))
	}
	return observed, nil
}
func (a *Adapter) Delete(ctx context.Context, expected runtime.ResourceSnapshot) error {
	if e := a.ready(ctx); e != nil {
		return e
	}
	resource := expected.Resource
	if e := validDelete(resource); e != nil {
		return bad("delete", e)
	}
	if len(expected.Labels) == 0 {
		return bad("delete", errors.New("expected ownership labels are missing"))
	}
	observed, e := a.inspectKind(ctx, resource.Kind, resource.ID)
	if e != nil {
		if missing(e, resourceNoun(resource.Kind), resource.Name) {
			return nil
		}
		return e
	}
	if observed.Resource != resource || !sameLabels(observed.Labels, expected.Labels) {
		return bad("delete", errors.New("typed resource or ownership labels do not match inspection"))
	}
	var args []string
	switch resource.Kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser:
		args = []string{"delete", string(resource.ID)}
	case runtime.ResourceNetwork:
		args = []string{"network", "delete", resource.Name}
	case runtime.ResourceVolume:
		args = []string{"volume", "delete", resource.Name}
	}
	if _, e = a.command(ctx, "delete", Command{Args: args}); e != nil {
		return e
	}
	if _, e = a.inspectKind(ctx, resource.Kind, resource.ID); e == nil {
		return unavailable("verify delete", errors.New("resource remains"))
	} else if !missing(e, resourceNoun(resource.Kind), resource.Name) {
		return e
	}
	return nil
}

func (a *Adapter) container(ctx context.Context, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	r, e := a.command(ctx, "inspect container", Command{Args: []string{"inspect", string(id)}})
	if e != nil {
		return runtime.ResourceSnapshot{}, e
	}
	v, e := decodeContainers(r.Stdout)
	if e != nil || len(v) != 1 {
		if e == nil {
			e = fmt.Errorf("expected one container, got %d", len(v))
		}
		return runtime.ResourceSnapshot{}, unavailable("decode container", e)
	}
	return v[0], nil
}
func (a *Adapter) inspectKind(ctx context.Context, k runtime.ResourceKind, id runtime.ResourceID) (runtime.ResourceSnapshot, error) {
	if k == runtime.ResourceWorkspace || k == runtime.ResourceBrowser {
		return a.container(ctx, id)
	}
	if k != runtime.ResourceNetwork && k != runtime.ResourceVolume {
		return runtime.ResourceSnapshot{}, bad("inspect", errors.New("unknown kind"))
	}
	r, e := a.command(ctx, "inspect "+string(k), Command{Args: []string{string(k), "inspect", string(id)}})
	if e != nil {
		return runtime.ResourceSnapshot{}, e
	}
	v, e := decodeNamed(r.Stdout, k)
	if e != nil || len(v) != 1 {
		if e == nil {
			e = fmt.Errorf("expected one resource, got %d", len(v))
		}
		return runtime.ResourceSnapshot{}, unavailable("decode "+string(k), e)
	}
	return v[0], nil
}

type appleContainerNetwork struct {
	Network string `json:"network"`
}

type appleContainerNetworkStatus struct {
	Network     string `json:"network"`
	IPv4Address string `json:"ipv4Address"`
	IPv6Address string `json:"ipv6Address"`
}

type appleContainer struct {
	ID            string `json:"id"`
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"image"`
		Mounts []struct {
			Type        json.RawMessage `json:"type"`
			Source      string          `json:"source"`
			Destination string          `json:"destination"`
			Options     []string        `json:"options"`
		} `json:"mounts"`
		Networks       []appleContainerNetwork `json:"networks"`
		PublishedPorts []struct {
			HostAddress   string `json:"hostAddress"`
			HostPort      uint16 `json:"hostPort"`
			ContainerPort uint16 `json:"containerPort"`
			Proto         string `json:"proto"`
			Count         uint16 `json:"count"`
		} `json:"publishedPorts"`
	} `json:"configuration"`
	Status struct {
		State    string                        `json:"state"`
		Networks []appleContainerNetworkStatus `json:"networks"`
	} `json:"status"`
}

func decodeContainers(b []byte) ([]runtime.ResourceSnapshot, error) {
	var x []appleContainer
	if e := decodeJSON(b, &x, false); e != nil {
		return nil, e
	}
	out := make([]runtime.ResourceSnapshot, 0, len(x))
	for _, v := range x {
		id := v.Configuration.ID
		if id == "" {
			id = v.ID
		}
		if id == "" {
			return nil, errors.New("empty container ID")
		}
		labels := labels(v.Configuration.Labels)
		s := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: runtime.ResourceID(id), Name: id, Kind: runtime.ResourceKind(label(labels, ownership.KindLabel))}, State: v.Status.State, Labels: labels}
		s.ImageDigest = v.Configuration.Image.Descriptor.Digest
		for _, mount := range v.Configuration.Mounts {
			source, mountKind := decodedMount(mount.Type, mount.Source)
			s.Mounts = append(s.Mounts, runtime.Mount{Source: source, Target: mount.Destination, ReadOnly: has(mount.Options, "ro"), Type: mountKind})
		}
		configuredNetworks := make(map[string]struct{}, len(v.Configuration.Networks))
		for _, n := range v.Configuration.Networks {
			if n.Network == "" {
				return nil, errors.New("empty configured network")
			}
			if _, duplicate := configuredNetworks[n.Network]; duplicate {
				return nil, fmt.Errorf("duplicate configured network %q", n.Network)
			}
			configuredNetworks[n.Network] = struct{}{}
			s.Networks = append(s.Networks, n.Network)
		}
		sort.Strings(s.Networks)
		networkAddresses, e := decodeNetworkAddresses(v.Status.Networks, configuredNetworks)
		if e != nil {
			return nil, e
		}
		s.NetworkAddresses = networkAddresses
		for _, p := range v.Configuration.PublishedPorts {
			ip, e := netip.ParseAddr(p.HostAddress)
			if e != nil {
				return nil, e
			}
			count := p.Count
			if count == 0 {
				count = 1
			}
			for o := uint16(0); o < count; o++ {
				s.Ports = append(s.Ports, runtime.PortBinding{HostIP: ip, HostPort: p.HostPort + o, GuestPort: p.ContainerPort + o, Protocol: p.Proto})
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeNetworkAddresses(statuses []appleContainerNetworkStatus, configured map[string]struct{}) (map[string][]netip.Addr, error) {
	addresses := make(map[string][]netip.Addr)
	statusNetworks := make(map[string]struct{}, len(statuses))
	addressNetworks := make(map[netip.Addr]string)
	for _, status := range statuses {
		if status.Network == "" {
			return nil, errors.New("empty status network")
		}
		if _, ok := configured[status.Network]; !ok {
			return nil, fmt.Errorf("status network %q is not configured", status.Network)
		}
		if _, duplicate := statusNetworks[status.Network]; duplicate {
			return nil, fmt.Errorf("duplicate status network %q", status.Network)
		}
		statusNetworks[status.Network] = struct{}{}
		for _, evidence := range []struct {
			value string
			ipv4  bool
		}{
			{status.IPv4Address, true},
			{status.IPv6Address, false},
		} {
			if evidence.value == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(evidence.value)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q for network %q: %w", evidence.value, status.Network, err)
			}
			address := prefix.Addr()
			if address.IsUnspecified() || evidence.ipv4 != address.Is4() || !evidence.ipv4 && (!address.Is6() || address.Is4In6()) {
				return nil, fmt.Errorf("invalid address family for %q on network %q", evidence.value, status.Network)
			}
			if otherNetwork, duplicate := addressNetworks[address]; duplicate {
				return nil, fmt.Errorf("address %q is duplicated on networks %q and %q", address, otherNetwork, status.Network)
			}
			addressNetworks[address] = status.Network
			addresses[status.Network] = append(addresses[status.Network], address)
		}
	}
	for network := range addresses {
		sort.Slice(addresses[network], func(i, j int) bool {
			return addresses[network][i].Compare(addresses[network][j]) < 0
		})
	}
	if len(addresses) == 0 {
		return nil, nil
	}
	return addresses, nil
}

type appleNamed struct {
	ID            string `json:"id"`
	Configuration struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
}

func decodeNamed(b []byte, k runtime.ResourceKind) ([]runtime.ResourceSnapshot, error) {
	var x []appleNamed
	if e := decodeJSON(b, &x, false); e != nil {
		return nil, e
	}
	out := make([]runtime.ResourceSnapshot, 0, len(x))
	for _, v := range x {
		name := v.Configuration.Name
		if name == "" {
			name = v.ID
		}
		if name == "" || v.ID != name {
			return nil, errors.New("named identity mismatch")
		}
		out = append(out, runtime.ResourceSnapshot{Resource: runtime.Resource{ID: runtime.ResourceID(v.ID), Name: name, Kind: k}, State: "created", Labels: labels(v.Configuration.Labels)})
	}
	return out, nil
}
func mountType(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s == "virtiofs" {
			return "bind"
		}
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) == nil {
		for _, s = range []string{"volume", "block", "virtiofs", "tmpfs"} {
			if _, ok := m[s]; ok {
				if s == "virtiofs" {
					return "bind"
				}
				return s
			}
		}
	}
	return "unknown"
}

func decodedMount(raw json.RawMessage, source string) (string, string) {
	kind := mountType(raw)
	if kind != "volume" {
		return source, kind
	}
	var typed struct {
		Volume struct {
			Name string `json:"name"`
		} `json:"volume"`
	}
	if json.Unmarshal(raw, &typed) == nil && typed.Volume.Name != "" {
		return typed.Volume.Name, kind
	}
	return source, kind
}

func validBuild(s runtime.ImageSpec) error {
	if e := validRef(s.Reference); e != nil {
		return e
	}
	if e := hostPath(s.Context); e != nil {
		return e
	}
	if e := hostPath(s.File); e != nil {
		return e
	}
	if s.Target != "" && !nameRE.MatchString(strings.ToLower(s.Target)) {
		return errors.New("invalid target")
	}
	if e := validLabels(s.BuildArgs, false); e != nil {
		return e
	}
	return validLabels(s.Labels, false)
}

func validWorkspace(s runtime.WorkspaceSpec) (runtime.ResourceKind, error) {
	if !nameRE.MatchString(s.Name) {
		return "", errors.New("invalid name")
	}
	if e := validRef(s.Image.Reference); e != nil {
		return "", e
	}
	if !digestRE.MatchString(s.Image.Digest) {
		return "", errors.New("unpinned digest")
	}
	if s.Image.Local {
		if _, e := pin(s.Image.Reference); e == nil {
			return "", errors.New("local image reference must not include a digest")
		}
	}
	if p, e := pin(s.Image.Reference); e == nil && p != s.Image.Digest {
		return "", errors.New("digest mismatch")
	}
	if len(s.Entrypoint) == 0 || len(s.Entrypoint) > maxItems {
		return "", errors.New("invalid entrypoint")
	}
	if e := argv(s.Entrypoint); e != nil {
		return "", e
	}
	if e := env(s.Env); e != nil {
		return "", e
	}
	if e := guestPath(s.WorkingDir); e != nil {
		return "", e
	}
	if e := nonroot(s.User); e != nil && !validRootSupervisor(s) {
		return "", e
	}
	if len(s.Mounts) > maxItems || len(s.Networks) > maxItems || len(s.Ports) > maxItems {
		return "", errors.New("too many resources")
	}
	for _, mount := range s.Mounts {
		if err := validMountAuthority(mount); err != nil {
			return "", err
		}
	}
	if err := validGuestHelperMounts(s.Mounts); err != nil {
		return "", err
	}
	if err := validSensitiveHostMounts(s.Mounts); err != nil {
		return "", err
	}
	for _, n := range s.Networks {
		if !nameRE.MatchString(n) {
			return "", errors.New("invalid network")
		}
	}
	for _, p := range s.Ports {
		if p.HostPort == nil {
			return "", errors.New("dynamic ports unsupported")
		}
		if p.HostIP != netip.MustParseAddr("127.0.0.1") {
			return "", errors.New("nonloopback ports unsupported")
		}
		if *p.HostPort == 0 || p.GuestPort == 0 || p.Protocol != "tcp" {
			return "", errors.New("invalid port")
		}
	}
	if s.CPUs < 0 || s.CPUs > 256 || s.MemoryBytes < 0 {
		return "", errors.New("invalid limits")
	}
	k := runtime.ResourceKind(label(s.Labels, ownership.KindLabel))
	if k != runtime.ResourceWorkspace {
		return "", errors.New("invalid kind")
	}
	if e := own(s.Name, k, s.Labels); e != nil {
		return "", e
	}
	return k, nil
}
func validBrowser(s runtime.BrowserSpec) error {
	if !nameRE.MatchString(s.Name) {
		return errors.New("invalid name")
	}
	if e := validRef(s.Image.Reference); e != nil {
		return e
	}
	if !digestRE.MatchString(s.Image.Digest) {
		return errors.New("unpinned digest")
	}
	if s.Image.Local {
		if _, e := pin(s.Image.Reference); e == nil {
			return errors.New("local image reference must not include a digest")
		}
	}
	if pinned, e := pin(s.Image.Reference); e == nil && pinned != s.Image.Digest {
		return errors.New("digest mismatch")
	}
	if len(s.Entrypoint) == 0 || len(s.Entrypoint) > maxItems {
		return errors.New("invalid entrypoint")
	}
	if e := argv(s.Entrypoint); e != nil {
		return e
	}
	if e := env(s.Env); e != nil {
		return e
	}
	if len(s.Networks) != 1 || !nameRE.MatchString(s.Networks[0]) {
		return errors.New("exactly one valid network required")
	}
	if s.CPUs < 0 || s.CPUs > maxCPUs {
		return errors.New("invalid CPU limit")
	}
	if s.MemoryBytes < 0 || s.MemoryBytes > maxMemoryBytes || (s.MemoryBytes > 0 && s.MemoryBytes < minMemoryBytes) {
		return errors.New("invalid memory limit")
	}
	if e := own(s.Name, runtime.ResourceBrowser, s.Labels); e != nil {
		return e
	}
	return nil
}

func verifyBrowserPostcondition(observed runtime.ResourceSnapshot, expected runtime.BrowserSpec) error {
	if observed.ID != runtime.ResourceID(expected.Name) || observed.Name != expected.Name || observed.Kind != runtime.ResourceBrowser {
		return errors.New("browser identity or kind mismatch")
	}
	if !sameLabels(observed.Labels, expected.Labels) {
		return errors.New("browser labels mismatch")
	}
	if observed.ImageDigest != expected.Image.Digest {
		return fmt.Errorf("browser image digest is %q, want %q", observed.ImageDigest, expected.Image.Digest)
	}
	if len(observed.Networks) != 1 || observed.Networks[0] != expected.Networks[0] {
		return errors.New("browser network attachment mismatch")
	}
	if len(observed.Mounts) != 0 {
		return fmt.Errorf("browser has %d mounts, want zero", len(observed.Mounts))
	}
	if len(observed.Ports) != 0 {
		return fmt.Errorf("browser has %d published ports, want zero", len(observed.Ports))
	}
	return nil
}
func verifyBrowserNetworkAddress(observed runtime.ResourceSnapshot) error {
	if len(observed.Networks) != 1 {
		return errors.New("browser owner-network attachment is missing or ambiguous")
	}
	addresses, ok := observed.NetworkAddresses[observed.Networks[0]]
	if !ok {
		return errors.New("browser owner-network address is missing")
	}
	for _, address := range addresses {
		if address.Is4() && address.IsPrivate() && !address.IsLoopback() && !address.IsUnspecified() {
			return nil
		}
	}
	return errors.New("browser owner network has no non-loopback private IPv4 address")
}

func validExec(s runtime.ExecSpec) error {
	if len(s.Argv) == 0 || len(s.Argv) > maxItems {
		return errors.New("invalid argv")
	}
	if e := argv(s.Argv); e != nil {
		return e
	}
	if e := env(s.Env); e != nil {
		return e
	}
	if s.WorkingDir != "" {
		if e := guestPath(s.WorkingDir); e != nil {
			return e
		}
	}
	if s.User != "" {
		if err := nonroot(s.User); err != nil && !validRootReadOnlyStaging(s) && !validRootReadOnlyCleanup(s) {
			return err
		}
	}
	return nil
}
func validRootReadOnlyStaging(spec runtime.ExecSpec) bool {
	if spec.User != "0:0" || spec.WorkingDir != "/workspace" || len(spec.Env) != 0 || len(spec.Argv) != 9 {
		return false
	}
	arguments := spec.Argv
	if arguments[0] != "/usr/local/libexec/dsx/dsx-guest" || arguments[1] != "stage-file" ||
		arguments[2] != "--read-only" || arguments[3] != "--child-uid" ||
		arguments[5] != "--child-gid" || arguments[7] != "--path" {
		return false
	}
	uid, uidErr := strconv.ParseUint(arguments[4], 10, 32)
	gid, gidErr := strconv.ParseUint(arguments[6], 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return false
	}
	name := arguments[8]
	if path.Clean(name) != name {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) < 5 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return false
	}
	runID, err := model.ParseRunID(parts[3])
	if err != nil || string(runID) != parts[3] {
		return false
	}
	for _, component := range parts[2:] {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "\x00\r\n") {
			return false
		}
	}
	return true
}

func validRootReadOnlyCleanup(spec runtime.ExecSpec) bool {
	if spec.User != "0:0" || spec.WorkingDir != "/workspace" || len(spec.Env) != 0 || len(spec.Argv) != 4 {
		return false
	}
	arguments := spec.Argv
	if arguments[0] != "/usr/local/libexec/dsx/dsx-guest" || arguments[1] != "remove-read-only" || arguments[2] != "--path" {
		return false
	}
	name := arguments[3]
	if path.Clean(name) != name {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "tmp" || parts[2] != "dsx-readonly" {
		return false
	}
	runID, err := model.ParseRunID(parts[3])
	return err == nil && string(runID) == parts[3]
}

func own(name string, k runtime.ResourceKind, l []runtime.Label) error {
	if len(l) != 7 {
		return errors.New("exactly seven ownership labels required")
	}
	if e := validLabels(l, true); e != nil {
		return e
	}
	m := map[string]string{}
	for _, v := range l {
		m[v.Key] = v.Value
	}
	p, e := model.ParseProjectID(m[ownership.ProjectLabel])
	if e != nil {
		return e
	}
	s, e := model.ParseSandboxName(m[ownership.SandboxLabel])
	if e != nil {
		return e
	}
	r, e := model.ParseRunID(m[ownership.RunLabel])
	if e != nil {
		return e
	}
	i, e := ownership.NewIdentity(p, s, r, k, m[ownership.RoleLabel])
	if e != nil {
		return e
	}
	if i.Name() != name || !sameLabels(i.Labels(), l) {
		return errors.New("ownership mismatch")
	}
	return nil
}
func validLabels(l []runtime.Label, only bool) error {
	if len(l) > maxItems {
		return errors.New("too many labels")
	}
	seen := map[string]bool{}
	for _, v := range l {
		if !labelRE.MatchString(v.Key) || v.Value == "" || len(v.Key)+len(v.Value) > 16384 || strings.ContainsAny(v.Value, "\x00\r\n") {
			return errors.New("invalid label")
		}
		if seen[v.Key] {
			return errors.New("duplicate label")
		}
		seen[v.Key] = true
		if only && !strings.HasPrefix(v.Key, "dev.dsx.") {
			return errors.New("foreign label")
		}
	}
	return nil
}
func env(v []string) error {
	if len(v) > maxItems {
		return errors.New("too much env")
	}
	seen := map[string]bool{}
	for _, s := range v {
		i := strings.IndexByte(s, '=')
		if i <= 0 || len(s) > 16384 || strings.ContainsAny(s, "\x00\r\n") {
			return errors.New("env must be KEY=VALUE")
		}
		k := s[:i]
		if !keyRE.MatchString(k) || seen[k] {
			return errors.New("invalid/duplicate env")
		}
		seen[k] = true
	}
	return nil
}
func argv(v []string) error {
	for _, s := range v {
		if s == "" || len(s) > 16384 || strings.IndexByte(s, 0) >= 0 {
			return errors.New("invalid argv")
		}
	}
	return nil
}
func hostPath(v runtime.HostPath) error {
	s := string(v)
	if s == "" || !filepath.IsAbs(s) || filepath.Clean(s) != s || strings.ContainsAny(s, "\x00\r\n:") {
		return errors.New("host path must be clean absolute")
	}
	parts := strings.Split(strings.TrimPrefix(s, "/"), "/")
	if (len(parts) == 2 && (parts[0] == "Users" || parts[0] == "home")) || s == "/root" {
		return errors.New("host home mount forbidden")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if filepath.Clean(home) == s {
			return errors.New("host home mount forbidden")
		}
		if sourceInfo, sourceErr := os.Stat(s); sourceErr == nil {
			if homeInfo, homeErr := os.Stat(home); homeErr == nil && os.SameFile(sourceInfo, homeInfo) {
				return errors.New("host home mount forbidden")
			}
		}
	}
	if strings.HasSuffix(strings.ToLower(s), ".sock") {
		return errors.New("runtime socket mount forbidden")
	}
	return nil
}

func hostMountPath(value runtime.HostPath) error {
	if err := hostPath(value); err != nil {
		return err
	}
	if err := config.ValidateHostMountPath(string(value)); err != nil {
		return err
	}
	return nil
}
func guestPath(v runtime.GuestPath) error {
	s := string(v)
	if s == "" || !strings.HasPrefix(s, "/") || path.Clean(s) != s || strings.ContainsAny(s, "\x00\r\n:") {
		return errors.New("guest path must be clean absolute")
	}
	if s == "/root" || strings.HasSuffix(strings.ToLower(s), ".sock") {
		return errors.New("guest home/socket forbidden")
	}
	return nil
}
func nonroot(s string) error {
	if s == "" || len(s) > 128 || strings.ContainsAny(s, "\x00\r\n ,") {
		return errors.New("non-root user required")
	}
	p := strings.SplitN(s, ":", 2)[0]
	if p == "root" || p == "0" {
		return errors.New("root forbidden")
	}
	return nil
}
func validRootSupervisor(spec runtime.WorkspaceSpec) bool {
	if spec.User != "0:0" {
		return false
	}
	want := []string{
		"/usr/local/libexec/dsx/dsx-guest", "serve",
		"--socket", "/run/dsx/control.sock",
		"--child-uid", "", "--child-gid", "",
	}
	if len(spec.Entrypoint) != len(want) && len(spec.Entrypoint) != len(want)+2 {
		return false
	}
	for index := range want {
		if want[index] != "" && spec.Entrypoint[index] != want[index] {
			return false
		}
	}
	uid, uidErr := strconv.ParseUint(spec.Entrypoint[5], 10, 32)
	gid, gidErr := strconv.ParseUint(spec.Entrypoint[7], 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return false
	}
	initializeWorkspace := len(spec.Entrypoint) == len(want)+2
	if initializeWorkspace && (spec.Entrypoint[8] != "--initialize-workspace" || spec.Entrypoint[9] != "/workspace") {
		return false
	}
	helperMounts := 0
	workspaceVolumes := 0
	for _, mount := range spec.Mounts {
		switch mount.Target {
		case "/usr/local/libexec/dsx":
			helperMounts++
			if mount.Type != "bind" || mount.Authority != runtime.MountAuthorityGuestHelper || !mount.ReadOnly ||
				hostPath(runtime.HostPath(mount.Source)) != nil || validGuestHelperHostDirectory(mount.Source) != nil {
				return false
			}
		case "/workspace":
			if mount.Type == "volume" && mount.Authority == runtime.MountAuthorityInternal && !mount.ReadOnly {
				workspaceVolumes++
			}
		}
	}
	if initializeWorkspace {
		return helperMounts == 1 && workspaceVolumes == 1
	}
	return helperMounts == 1
}

func validGuestHelperMounts(mounts []runtime.Mount) error {
	const helperRoot = "/usr/local/libexec/dsx"
	for _, mount := range mounts {
		if !guestPathsOverlap(mount.Target, helperRoot) {
			continue
		}
		if mount.Target != helperRoot || mount.Type != "bind" || mount.Authority != runtime.MountAuthorityGuestHelper ||
			!mount.ReadOnly || hostPath(runtime.HostPath(mount.Source)) != nil || validGuestHelperHostDirectory(mount.Source) != nil {
			return errors.New("mount overlaps reserved guest helper directory")
		}
	}
	return nil
}

func validSensitiveHostMounts(mounts []runtime.Mount) error {
	const leappGuestDirectory = "/run/dsx/aws"
	sensitiveSource := ""
	for _, mount := range mounts {
		if !guestPathsOverlap(mount.Target, leappGuestDirectory) {
			continue
		}
		if sensitiveSource != "" || mount.Target != leappGuestDirectory || mount.Type != "bind" ||
			mount.Authority != runtime.MountAuthorityLeappMirror || !mount.ReadOnly {
			return errors.New("mount overlaps reserved sensitive directory")
		}
		sensitiveSource = mount.Source
	}
	if sensitiveSource == "" {
		return nil
	}
	writable := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Type == "bind" && !mount.ReadOnly {
			writable = append(writable, mount.Source)
		}
	}
	if err := hostsource.ValidateReadOnlyIsolation(sensitiveSource, writable); err != nil {
		return errors.New("sensitive read-only host source overlaps writable workspace authority")
	}
	return nil
}

func validGuestHelperHostDirectory(directory string) error {
	if resolved, err := filepath.EvalSymlinks(directory); err != nil || resolved != directory {
		return errors.New("guest helper directory must be canonical and symlink-free")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return errors.New("guest helper directory must be DSX-owned mode 0700")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "dsx-guest" || entries[0].Type()&os.ModeSymlink != 0 {
		return errors.New("guest helper directory must contain only dsx-guest")
	}
	helper, err := os.Lstat(filepath.Join(directory, "dsx-guest"))
	if err != nil || !helper.Mode().IsRegular() || helper.Mode().Perm() != 0o700 || !ownedByCurrentUser(helper) {
		return errors.New("guest helper must be a DSX-owned mode 0700 regular file")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(metadata.Uid) == os.Getuid() && int(metadata.Gid) == os.Getgid()
}

func guestPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func validID(id runtime.ResourceID) error {
	if !nameRE.MatchString(string(id)) {
		return errors.New("invalid resource ID")
	}
	return nil
}
func validDelete(r runtime.Resource) error {
	if r.ID == "" || r.Name == "" {
		return errors.New("empty resource")
	}
	if r.Name == "buildkit" || strings.HasPrefix(r.Name, "container-builder") || string(r.ID) == "buildkit" || strings.HasPrefix(string(r.ID), "container-builder") {
		return errors.New("builder delete forbidden")
	}
	switch r.Kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser:
		if e := validID(r.ID); e != nil {
			return e
		}
	case runtime.ResourceNetwork:
		if !nameRE.MatchString(r.Name) {
			return errors.New("invalid network")
		}
	case runtime.ResourceVolume:
		if !volumeRE.MatchString(r.Name) || len(r.Name) > 63 {
			return errors.New("invalid volume")
		}
	default:
		return errors.New("unknown kind")
	}
	if string(r.ID) != r.Name {
		return errors.New("ID/name mismatch")
	}
	return nil
}
func validRef(s string) error {
	if s == "" || len(s) > 512 || strings.ContainsAny(s, "\x00\r\n\t ") || !refRE.MatchString(s) {
		return errors.New("invalid image reference")
	}
	return nil
}
func pin(s string) (string, error) {
	if e := validRef(s); e != nil {
		return "", e
	}
	i := strings.LastIndexByte(s, '@')
	if i < 1 || !digestRE.MatchString(s[i+1:]) {
		return "", errors.New("image must be sha256 pinned")
	}
	return s[i+1:], nil
}
func resourceNoun(kind runtime.ResourceKind) string {
	switch kind {
	case runtime.ResourceWorkspace, runtime.ResourceBrowser:
		return "container"
	case runtime.ResourceNetwork:
		return "network"
	case runtime.ResourceVolume:
		return "volume"
	default:
		return ""
	}
}
func imageRef(i runtime.Image) string {
	if i.Local {
		return i.Reference
	}
	if _, e := pin(i.Reference); e == nil {
		return i.Reference
	}
	return i.Reference + "@" + i.Digest
}
func signal(s runtime.Signal) (string, error) {
	v := strings.TrimPrefix(strings.ToUpper(string(s)), "SIG")
	switch v {
	case "HUP", "INT", "QUIT", "KILL", "TERM", "USR1", "USR2":
		return v, nil
	}
	return "", errors.New("unsupported signal")
}
func validMountAuthority(m runtime.Mount) error {
	switch m.Authority {
	case runtime.MountAuthorityRepository:
		if m.Type != "bind" || m.Target != "/workspace" && !strings.HasPrefix(m.Target, "/workspace/") {
			return errors.New("repository mount authority has an invalid type or target")
		}
		return hostPath(runtime.HostPath(m.Source))
	case runtime.MountAuthorityConfiguredHost:
		if m.Type != "bind" {
			return errors.New("configured host mount authority requires a bind")
		}
		return hostMountPath(runtime.HostPath(m.Source))
	case runtime.MountAuthorityLeappMirror:
		if m.Type != "bind" || m.Target != "/run/dsx/aws" || !m.ReadOnly {
			return errors.New("Leapp mirror authority has an invalid type, target, or mode")
		}
		return hostPath(runtime.HostPath(m.Source))
	case runtime.MountAuthorityGuestHelper:
		if m.Type != "bind" || m.Target != "/usr/local/libexec/dsx" || !m.ReadOnly {
			return errors.New("guest helper authority has an invalid type, target, or mode")
		}
		if err := hostPath(runtime.HostPath(m.Source)); err != nil {
			return err
		}
		return validGuestHelperHostDirectory(m.Source)
	case runtime.MountAuthorityInternal:
		if m.Type != "volume" || m.Target != "/workspace" || m.ReadOnly {
			return errors.New("internal artifact authority has an invalid type, target, or mode")
		}
		return nil
	case runtime.MountAuthorityVolume:
		if m.Type != "volume" {
			return errors.New("volume authority requires a volume mount")
		}
		return nil
	default:
		return errors.New("mount authority is missing or unsupported")
	}
}

func mountArg(m runtime.Mount) (string, error) {
	if e := guestPath(runtime.GuestPath(m.Target)); e != nil {
		return "", e
	}
	if strings.ContainsAny(m.Source+m.Target, ",=") {
		return "", errors.New("mount delimiter")
	}
	if err := validMountAuthority(m); err != nil {
		return "", err
	}
	var v string
	switch m.Type {
	case "bind":
		v = "type=bind,source=" + m.Source + ",target=" + m.Target
	case "volume":
		if !volumeRE.MatchString(m.Source) || len(m.Source) > 63 {
			return "", errors.New("invalid volume")
		}
		v = "type=volume,source=" + m.Source + ",target=" + m.Target
	default:
		return "", errors.New("unsupported mount")
	}
	if m.ReadOnly {
		v += ",readonly"
	}
	return v, nil
}
func labelArgs(a []string, l []runtime.Label) []string {
	for _, v := range l {
		a = append(a, "--label", v.Key+"="+v.Value)
	}
	return a
}
func sameLabels(a, b []runtime.Label) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]string{}
	for _, v := range a {
		if _, ok := m[v.Key]; ok {
			return false
		}
		m[v.Key] = v.Value
	}
	for _, v := range b {
		if m[v.Key] != v.Value {
			return false
		}
	}
	return true
}
func labels(m map[string]string) []runtime.Label {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	r := make([]runtime.Label, 0, len(k))
	for _, s := range k {
		r = append(r, runtime.Label{Key: s, Value: m[s]})
	}
	return r
}
func label(l []runtime.Label, k string) string {
	for _, v := range l {
		if v.Key == k {
			return v.Value
		}
	}
	return ""
}
func has(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}

var _ runtime.Adapter = (*Adapter)(nil)
