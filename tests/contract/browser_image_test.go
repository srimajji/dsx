package contract_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type browserPackageManifest struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Private      bool              `json:"private"`
	Dependencies map[string]string `json:"dependencies"`
}

type browserPackageLock struct {
	Name            string                        `json:"name"`
	Version         string                        `json:"version"`
	LockfileVersion int                           `json:"lockfileVersion"`
	Requires        bool                          `json:"requires"`
	Packages        map[string]browserLockPackage `json:"packages"`
}

type browserLockPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved"`
	Integrity            string            `json:"integrity"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Bin                  map[string]string `json:"bin"`
	Engines              map[string]string `json:"engines"`
	HasInstallScript     bool              `json:"hasInstallScript"`
	Optional             bool              `json:"optional"`
	OS                   []string          `json:"os"`
}

func TestBrowserImageRecipeAndLockContract(t *testing.T) {
	root := filepath.Join("..", "..")
	browserDir := filepath.Join(root, "images", "browser")

	manifestData, err := os.ReadFile(filepath.Join(browserDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest browserPackageManifest
	decodeBrowserJSON(t, manifestData, &manifest)
	wantRootDependency := map[string]string{"@playwright/mcp": "0.0.79"}
	if manifest.Name != "dsx-browser-image" || manifest.Version != "0.0.0" || !manifest.Private || !maps.Equal(manifest.Dependencies, wantRootDependency) {
		t.Fatalf("unexpected browser package manifest: %#v", manifest)
	}

	lockData, err := os.ReadFile(filepath.Join(browserDir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock browserPackageLock
	decodeBrowserJSON(t, lockData, &lock)
	if lock.Name != manifest.Name || lock.Version != manifest.Version || lock.LockfileVersion != 3 || !lock.Requires {
		t.Fatalf("invalid browser package lock header: %#v", lock)
	}
	if len(lock.Packages) != 5 {
		t.Fatalf("locked package count = %d, want 5", len(lock.Packages))
	}
	rootPackage, found := lock.Packages[""]
	if !found || rootPackage.Name != manifest.Name || rootPackage.Version != manifest.Version || !maps.Equal(rootPackage.Dependencies, manifest.Dependencies) {
		t.Fatalf("package-lock root does not match package.json: %#v", rootPackage)
	}

	wantArtifacts := map[string]struct {
		version   string
		resolved  string
		integrity string
	}{
		"node_modules/@playwright/mcp": {
			version:   "0.0.79",
			resolved:  "https://registry.npmjs.org/@playwright/mcp/-/mcp-0.0.79.tgz",
			integrity: "sha512-VpqD4a3vFyGQMY9sh3UJiO6wjcurggkljKfAyCHL0QWGY5m6Ehr3MNsAAHPDHO//n13g0PCjpHatAOiulrqdZQ==",
		},
		"node_modules/fsevents": {
			version:   "2.3.2",
			resolved:  "https://registry.npmjs.org/fsevents/-/fsevents-2.3.2.tgz",
			integrity: "sha512-xiqMQR4xAeHTuB9uWm+fFRcIOgKBMiOBP+eXiyT7jsgVCq1bkVygt00oASowB7EdtpOHaaPgKt812P9ab+DDKA==",
		},
		"node_modules/playwright": {
			version:   "1.63.0-alpha-2026-08-05",
			resolved:  "https://registry.npmjs.org/playwright/-/playwright-1.63.0-alpha-2026-08-05.tgz",
			integrity: "sha512-zbGZUK+JYkoDV3cUgfvh2czTBJL34Gmz5gHVI25xiIpvYSR17Q1M7TS8hnwECUe+IkKaeXbKrSyJTyogm2DVWw==",
		},
		"node_modules/playwright-core": {
			version:   "1.63.0-alpha-2026-08-05",
			resolved:  "https://registry.npmjs.org/playwright-core/-/playwright-core-1.63.0-alpha-2026-08-05.tgz",
			integrity: "sha512-YussvUybTfBtyYbGXWh43f+5kNP03wg98M6mu4DphYET7PSbNVajsdLGjWE1xrsjqOw32i2wFlRP7U5mcOpMZg==",
		},
	}
	for path, want := range wantArtifacts {
		got, found := lock.Packages[path]
		if !found {
			t.Fatalf("lock is missing %q", path)
		}
		if got.Version != want.version || got.Resolved != want.resolved || got.Integrity != want.integrity {
			t.Fatalf("unexpected lock for %q: version=%q resolved=%q integrity=%q", path, got.Version, got.Resolved, got.Integrity)
		}
	}

	assertExactDependencies(t, lock.Packages["node_modules/@playwright/mcp"].Dependencies, map[string]string{
		"playwright":      "1.63.0-alpha-2026-08-05",
		"playwright-core": "1.63.0-alpha-2026-08-05",
	})
	assertExactDependencies(t, lock.Packages["node_modules/playwright"].Dependencies, map[string]string{
		"playwright-core": "1.63.0-alpha-2026-08-05",
	})
	assertExactDependencies(t, lock.Packages["node_modules/playwright"].OptionalDependencies, map[string]string{
		"fsevents": "2.3.2",
	})
	if fsevents := lock.Packages["node_modules/fsevents"]; !fsevents.Optional || len(fsevents.OS) != 1 || fsevents.OS[0] != "darwin" {
		t.Fatalf("fsevents must remain a Darwin-only optional lock entry: %#v", fsevents)
	}

	containerfileData, err := os.ReadFile(filepath.Join(browserDir, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	entrypointData, err := os.ReadFile(filepath.Join(browserDir, "entrypoint.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	containerfile := string(containerfileData)
	const base = "FROM mcr.microsoft.com/playwright@sha256:5361940f845a5077926d54746122f7b68a121cc2aa27df6241087b774203fc44"
	const entrypoint = "ENTRYPOINT [\"/usr/bin/node\", \"/app/entrypoint.mjs\"]"
	if !strings.HasPrefix(containerfile, base+"\n") {
		t.Fatal("browser base is not pinned to the approved Microsoft Playwright ARM64 noble digest")
	}
	if strings.Count(containerfile, "FROM ") != 1 ||
		strings.Count(containerfile, "COPY ") != 1 ||
		!strings.Contains(containerfile, "\nCOPY package.json package-lock.json entrypoint.mjs ./\n") {
		t.Fatal("browser recipe must copy only the pinned npm manifest, lock, and bounded entrypoint")
	}
	if strings.Count(containerfile, "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --omit=dev --ignore-scripts --no-audit --no-fund") != 1 {
		t.Fatal("browser dependencies must be installed exactly once at build time without browser downloads or package scripts")
	}
	if strings.Count(containerfile, entrypoint) != 1 || !strings.HasSuffix(containerfile, entrypoint+"\n") {
		t.Fatal("browser image entrypoint is not the fixed headless isolated Chromium MCP listener")
	}
	if !strings.Contains(string(entrypointData), "\"--allowed-hosts\", allowedAuthority") {
		t.Fatal("browser entrypoint must bind Host validation to its sole inspected private IPv4 address and MCP port")
	}
	if !strings.Contains(containerfile, "\nUSER pwuser\nWORKDIR /home/pwuser\n") {
		t.Fatal("browser MCP must run as the unprivileged image user from its writable home")
	}

	allArtifacts := strings.ToLower(string(manifestData) + "\n" + string(lockData) + "\n" + containerfile + "\n" + string(entrypointData))
	for _, forbidden := range []string{
		"dsx-guest", "harness", "omp", "codex", "claude", "opencode",
		"credential", "_auth", ".aws", "aws", "provider", "secret",
		"/workspace", "/source", "/src", ".git", "mount", "volume", "socket", ".sock",
		"expose ", "--publish", "\nadd ", "\ncmd ", " npx ", "npm install", "npm exec",
	} {
		if strings.Contains(allArtifacts, forbidden) {
			t.Fatalf("browser image artifacts contain forbidden credential, source, helper, or runtime-install content %q", forbidden)
		}
	}
}

func decodeBrowserJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}

func assertExactDependencies(t *testing.T, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Fatalf("dependency lock = %v, want %v", got, want)
	}
	for name, version := range got {
		if version == "" || strings.ContainsAny(version, "~^*<>=| ") {
			t.Fatalf("dependency %q has floating version %q", name, version)
		}
	}
}
