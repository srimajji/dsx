package contract_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	releaseVersion = "1.2.3"
	releaseCommit  = "0123456789abcdef0123456789abcdef01234567"
	releaseBuiltAt = "2026-08-10T00:00:00Z"
	releaseAgent   = "ghcr.io/srimajji/dsx-agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	releaseBrowser = "ghcr.io/srimajji/dsx-browser@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestReleaseArtifactManifestIsDeterministicAndFailClosed(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required by release tooling")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate release contract test")
	}
	tool := filepath.Join(filepath.Dir(source), "..", "..", "scripts", "release", "artifacts.py")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := make([]byte, 64)
	copy(host, []byte{0xcf, 0xfa, 0xed, 0xfe})
	binary.LittleEndian.PutUint32(host[4:8], 0x0100000c)
	guest := make([]byte, 64)
	copy(guest, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	binary.LittleEndian.PutUint16(guest[18:20], 183)
	for name, value := range map[string][]byte{
		"bin/dsx": host, "bin/dsx-guest": guest, "dsx.spdx.json": []byte(`{"spdxVersion":"SPDX-2.3","name":"dsx-1.2.3-darwin-arm64","creationInfo":{"created":"2026-08-10T00:00:00Z"}}`),
	} {
		if err := os.WriteFile(filepath.Join(root, name), value, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	guestSum := sha256.Sum256(guest)
	manifestA := filepath.Join(root, "release-manifest.json")
	manifestB := filepath.Join(root, "release-manifest-copy.json")
	manifestArgs := []string{tool, "manifest", "--root", root, "--version", releaseVersion, "--commit", releaseCommit,
		"--built-at", releaseBuiltAt, "--guest-sha256", hex.EncodeToString(guestSum[:]), "--agent-image", releaseAgent,
		"--browser-image", releaseBrowser, "--syft-version", "1.29.0"}
	runReleaseTool(t, python, append(append([]string(nil), manifestArgs...), "--output", manifestA)...)
	runReleaseTool(t, python, append(append([]string(nil), manifestArgs...), "--output", manifestB)...)
	first, err := os.ReadFile(manifestA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(manifestB)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("identical release inputs produced different manifests")
	}
	archiveRoot := t.TempDir()
	archiveA := filepath.Join(archiveRoot, "a.zip")
	archiveB := filepath.Join(archiveRoot, "b.zip")
	runReleaseTool(t, python, tool, "archive", "--root", root, "--output", archiveA)
	runReleaseTool(t, python, tool, "archive", "--root", root, "--output", archiveB)
	firstArchive, err := os.ReadFile(archiveA)
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := os.ReadFile(archiveB)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(firstArchive) != sha256.Sum256(secondArchive) {
		t.Fatal("identical release inputs produced different archives")
	}

	metadata := filepath.Join(root, "host-version.json")
	if err := os.WriteFile(metadata, []byte(`{"version":"1.2.3","commit":"0123456789abcdef0123456789abcdef01234567","built_at":"2026-08-10T00:00:00Z","guest_sha256":"`+hex.EncodeToString(guestSum[:])+`","agent_image":"`+releaseAgent+`","browser_image":"`+releaseBrowser+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	verify := []string{tool, "verify", "--root", root, "--manifest", manifestA, "--host-metadata", metadata,
		"--expected-version", releaseVersion, "--expected-commit", releaseCommit}
	runReleaseTool(t, python, verify...)

	t.Run("guest tamper", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "bin/dsx-guest"), append(guest, 'x'), 0o755); err != nil {
			t.Fatal(err)
		}
		runReleaseToolFails(t, python, verify, "digest or size mismatch")
		if err := os.WriteFile(filepath.Join(root, "bin/dsx-guest"), guest, 0o755); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing image pin", func(t *testing.T) {
		mutateManifest(t, manifestA, func(value map[string]any) { value["images"].(map[string]any)["agent"] = "unknown" })
		runReleaseToolFails(t, python, verify, "immutable registry reference")
		if err := os.WriteFile(manifestA, first, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("local image is not a published pin", func(t *testing.T) {
		mutateManifest(t, manifestA, func(value map[string]any) {
			value["images"].(map[string]any)["browser"] = "dsx.local/browser:mvp@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		})
		runReleaseToolFails(t, python, verify, "immutable registry reference")
		if err := os.WriteFile(manifestA, first, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		mutateManifest(t, manifestA, func(value map[string]any) { value["build"].(map[string]any)["version"] = "9.9.9" })
		runReleaseToolFails(t, python, verify, "expected version")
		if err := os.WriteFile(manifestA, first, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong host architecture", func(t *testing.T) {
		wrong := append([]byte(nil), host...)
		binary.LittleEndian.PutUint32(wrong[4:8], 0x01000007)
		if err := os.WriteFile(filepath.Join(root, "bin/dsx"), wrong, 0o755); err != nil {
			t.Fatal(err)
		}
		args := append(append([]string(nil), manifestArgs...), "--output", manifestB)
		runReleaseToolFails(t, python, args, "expected 64-bit arm64 Mach-O")
		if err := os.WriteFile(filepath.Join(root, "bin/dsx"), host, 0o755); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReleaseSecurityEvidenceRejectsUnsignedCandidate(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required by release tooling")
	}
	_, source, _, _ := runtime.Caller(0)
	tool := filepath.Join(filepath.Dir(source), "..", "..", "scripts", "release", "artifacts.py")
	root := t.TempDir()
	details := filepath.Join(root, "codesign.txt")
	notary := filepath.Join(root, "notary.json")
	if err := os.WriteFile(details, []byte("Executable=dsx\nSignature=adhoc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notary, []byte(`{"status":"Accepted","id":"submission-id"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runReleaseToolFails(t, python, []string{tool, "verify-security", "--codesign-details", details, "--notarization-result", notary}, "unsigned, ad-hoc signed")
}

func TestReleasePreflightRequiresRegistryAndCredentialInputs(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	repository := filepath.Join(filepath.Dir(source), "..", "..")
	dryRun := filepath.Join(repository, "scripts", "release", "build.sh")
	command := exec.Command(dryRun)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"VERSION=1.2.3",
		"COMMIT=" + releaseCommit,
		"SOURCE_DATE_EPOCH=1786320000",
		"DSX_AGENT_IMAGE=",
		"DSX_BROWSER_IMAGE=" + releaseBrowser,
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "DSX_AGENT_IMAGE") {
		t.Fatalf("missing registry pin result = %v, %q", err, output)
	}

	unsigned := t.TempDir()
	if err := os.Mkdir(filepath.Join(unsigned, "package"), 0o755); err != nil {
		t.Fatal(err)
	}
	sign := filepath.Join(repository, "scripts", "release", "sign-notarize.sh")
	command = exec.Command(sign, unsigned)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "SIGNING_IDENTITY=", "NOTARY_KEYCHAIN_PROFILE="}
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "SIGNING_IDENTITY") {
		t.Fatalf("missing signing identity result = %v, %q", err, output)
	}
}

func mutateManifest(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runReleaseTool(t *testing.T, python string, arguments ...string) {
	t.Helper()
	command := exec.Command(python, arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release tool failed: %v\n%s", err, output)
	}
}

func runReleaseToolFails(t *testing.T, python string, arguments []string, expected string) {
	t.Helper()
	command := exec.Command(python, arguments...)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("release tool unexpectedly succeeded")
	}
	if !strings.Contains(string(output), expected) {
		t.Fatalf("release failure = %q, want %q", output, expected)
	}
}
