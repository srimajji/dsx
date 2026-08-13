package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/harness/catalog"
)

type harnessImageLock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Platform      string `json:"platform"`
	Base          struct {
		Reference string `json:"reference"`
		Release   string `json:"release"`
	} `json:"base"`
	Harnesses []struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		Source         string `json:"source"`
		UpstreamDigest string `json:"upstreamDigest"`
		BuildSHA256    string `json:"buildSha256"`
		Executable     string `json:"executable"`
	} `json:"harnesses"`
}

type shellToolchainsLock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Platform      string `json:"platform"`
	Base          struct {
		Reference string `json:"reference"`
		Release   string `json:"release"`
	} `json:"base"`
	APT struct {
		Snapshot  string `json:"snapshot"`
		Bootstrap struct {
			Repository string `json:"repository"`
			Package    string `json:"package"`
			Version    string `json:"version"`
			Trust      string `json:"trust"`
		} `json:"bootstrap"`
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	} `json:"apt"`
	Artifacts []struct {
		Name                    string `json:"name"`
		Version                 string `json:"version"`
		Source                  string `json:"source"`
		SHA256                  string `json:"sha256"`
		InstallRoot             string `json:"installRoot"`
		UpstreamDigestAlgorithm string `json:"upstreamDigestAlgorithm"`
		UpstreamDigest          string `json:"upstreamDigest"`
		Signature               string `json:"signature"`
		SigningKeyFingerprint   string `json:"signingKeyFingerprint"`
	} `json:"artifacts"`
	Plugins struct {
		Manager struct {
			Name     string `json:"name"`
			Version  string `json:"version"`
			Revision string `json:"revision"`
			Source   string `json:"source"`
			SHA256   string `json:"sha256"`
		} `json:"manager"`
		Entries []struct {
			Repository string `json:"repository"`
			Revision   string `json:"revision"`
			Source     string `json:"source"`
			SHA256     string `json:"sha256"`
			Phase      string `json:"phase"`
			Order      int    `json:"order"`
		} `json:"entries"`
	} `json:"plugins"`
	Tools []struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Provider   string `json:"provider"`
		Executable string `json:"executable"`
	} `json:"tools"`
	Environment struct {
		Shell          string `json:"shell"`
		Path           string `json:"path"`
		JavaHome       string `json:"javaHome"`
		DotnetRoot     string `json:"dotnetRoot"`
		KotlinHome     string `json:"kotlinHome"`
		StarshipConfig string `json:"starshipConfig"`
		ZDOTDir        string `json:"zdotdir"`
	} `json:"environment"`
	Generated struct {
		PluginsPre      string `json:"pluginsPre"`
		PluginsPost     string `json:"pluginsPost"`
		CompletionCache string `json:"completionCache"`
		FZFInit         string `json:"fzfInit"`
		DirenvInit      string `json:"direnvInit"`
		StarshipInit    string `json:"starshipInit"`
	} `json:"generated"`
}

func isLowerHex(value string, length int) bool {
	return len(value) == length && strings.Trim(value, "0123456789abcdef") == ""
}

func TestHarnessImageLockMatchesAdaptersAndBuildRecipe(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "images", "agent", "harnesses.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock harnessImageLock
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	if lock.SchemaVersion != 1 || lock.Platform != "linux/arm64" || lock.Base.Release != "26.04" ||
		lock.Base.Reference != "docker.io/library/ubuntu@sha256:3fe5b610f5c41eeeb56c2995bd4afb4990ac5b80dc980e33f9251eaaa8013615" {
		t.Fatalf("invalid harness image lock header: %#v", lock)
	}
	byName := make(map[harness.Name]harness.PinnedArtifact)
	for _, adapter := range catalog.All() {
		byName[adapter.Name()] = adapter.Version()
	}
	containerfile, err := os.ReadFile(filepath.Join(root, "images", "agent", "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, len(lock.Harnesses))
	for _, entry := range lock.Harnesses {
		name, err := harness.ParseName(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		artifact, found := byName[name]
		if !found || artifact.Version != entry.Version || artifact.Source != entry.Source || artifact.Digest != entry.UpstreamDigest || artifact.Executable != entry.Executable {
			t.Fatalf("lock mismatch for %q: entry=%#v adapter=%#v", name, entry, artifact)
		}
		if len(entry.BuildSHA256) != 64 || !strings.Contains(string(containerfile), "--checksum=sha256:"+entry.BuildSHA256+" "+entry.Source) {
			t.Fatalf("build recipe does not checksum %q", name)
		}
		seen = append(seen, entry.Name)
	}
	sort.Strings(seen)
	if strings.Join(seen, ",") != "claude,codex,omp,opencode" {
		t.Fatalf("locked harnesses = %v", seen)
	}
	if !strings.Contains(string(containerfile), "FROM "+lock.Base.Reference) {
		t.Fatal("Containerfile base does not match lock")
	}
}

func TestShellToolchainsLockMatchesBuildRecipe(t *testing.T) {
	root := filepath.Join("..", "..")
	imageRoot := filepath.Join(root, "images", "agent")
	data, err := os.ReadFile(filepath.Join(imageRoot, "shell-toolchains.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock shellToolchainsLock
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	if lock.SchemaVersion != 1 || lock.Platform != "linux/arm64" || lock.Base.Release != "26.04" ||
		lock.Base.Reference != "docker.io/library/ubuntu@sha256:3fe5b610f5c41eeeb56c2995bd4afb4990ac5b80dc980e33f9251eaaa8013615" {
		t.Fatalf("invalid shell toolchains lock header: %#v", lock)
	}

	containerfile, err := os.ReadFile(filepath.Join(imageRoot, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	recipe := string(containerfile)
	if !strings.Contains(recipe, "FROM "+lock.Base.Reference) {
		t.Fatal("Containerfile base does not match shell toolchains lock")
	}
	if lock.APT.Snapshot != "https://snapshot.ubuntu.com/ubuntu/20260811T000000Z/" || !strings.Contains(recipe, lock.APT.Snapshot) {
		t.Fatalf("Containerfile does not use locked apt snapshot %q", lock.APT.Snapshot)
	}
	bootstrap := lock.APT.Bootstrap
	if bootstrap.Repository != "http://ports.ubuntu.com/ubuntu-ports" ||
		bootstrap.Package != "ca-certificates" ||
		bootstrap.Version != "20260601~26.04.1" ||
		bootstrap.Trust != "apt-signed-index" {
		t.Fatalf("invalid apt bootstrap contract: %#v", bootstrap)
	}
	bootstrapPackage := bootstrap.Package + "=" + bootstrap.Version
	canonicalRecipe := strings.Join(strings.Fields(strings.ReplaceAll(recipe, "\\\n", " ")), " ")
	updateAt := strings.Index(canonicalRecipe, "apt-get update")
	bootstrapAt := strings.Index(canonicalRecipe, "apt-get install -y --no-install-recommends "+bootstrapPackage+" &&")
	snapshotAt := strings.Index(canonicalRecipe, lock.APT.Snapshot)
	if !strings.Contains(recipe, bootstrap.Repository) || updateAt < 0 || bootstrapAt <= updateAt || snapshotAt <= bootstrapAt {
		t.Fatal("Containerfile must use the signed Ubuntu archive to install the exact CA bootstrap before rewriting to the HTTPS snapshot")
	}
	if strings.Count(recipe, bootstrapPackage) != 1 {
		t.Fatalf("Containerfile must install the exact CA bootstrap once, found %d occurrences", strings.Count(recipe, bootstrapPackage))
	}
	snapshotUpdateAt := strings.Index(canonicalRecipe[snapshotAt:], "apt-get update")
	snapshotInstallAt := strings.Index(canonicalRecipe[snapshotAt:], "apt-get install -y --no-install-recommends")
	if snapshotUpdateAt < 0 || snapshotInstallAt <= snapshotUpdateAt {
		t.Fatal("Containerfile must update and install packages normally after the HTTPS snapshot rewrite")
	}

	providerVersions := make(map[string]string)
	packageNames := make([]string, 0, len(lock.APT.Packages))
	for _, pkg := range lock.APT.Packages {
		_, duplicate := providerVersions[pkg.Name]
		if pkg.Name == "" || pkg.Version == "" || duplicate {
			t.Fatalf("invalid or duplicate apt package: %#v", pkg)
		}
		if pkg.Name == bootstrap.Package {
			t.Fatalf("bootstrap package %q must not be duplicated in snapshot packages", bootstrap.Package)
		}
		providerVersions[pkg.Name] = pkg.Version
		packageNames = append(packageNames, pkg.Name)
		if !strings.Contains(recipe, pkg.Name+"="+pkg.Version) {
			t.Fatalf("Containerfile does not install locked apt package %s=%s", pkg.Name, pkg.Version)
		}
	}
	sort.Strings(packageNames)
	const expectedPackages = "bat,build-essential,curl,git,groff,jq,less,libatomic1,libc6,libgcc-s1,libgssapi-krb5-2,libicu78,libssl3t64,libstdc++6,openssh-client,python3,python3-pip,python3-venv,ripgrep,sudo,tmux,tzdata,tzdata-legacy,unzip,zip,zlib1g,zsh"
	if strings.Join(packageNames, ",") != expectedPackages {
		t.Fatalf("locked apt packages = %v", packageNames)
	}

	artifactNames := make([]string, 0, len(lock.Artifacts))
	for _, artifact := range lock.Artifacts {
		_, duplicate := providerVersions[artifact.Name]
		if artifact.Name == "" || artifact.Version == "" || artifact.Source == "" || artifact.InstallRoot == "" || duplicate {
			t.Fatalf("invalid or duplicate artifact: %#v", artifact)
		}
		if !isLowerHex(artifact.SHA256, 64) || !strings.Contains(recipe, "--checksum=sha256:"+artifact.SHA256+" "+artifact.Source) {
			t.Fatalf("Containerfile does not checksum locked artifact %q", artifact.Name)
		}
		if !strings.Contains(recipe, artifact.InstallRoot) {
			t.Fatalf("Containerfile does not contain install root %q for %q", artifact.InstallRoot, artifact.Name)
		}
		switch artifact.Name {
		case "dotnet-sdk":
			if artifact.UpstreamDigestAlgorithm != "sha512" || !isLowerHex(artifact.UpstreamDigest, 128) ||
				artifact.Signature != "" || artifact.SigningKeyFingerprint != "" {
				t.Fatalf("invalid .NET publisher provenance: %#v", artifact)
			}
		case "aws-cli-v2":
			if artifact.Signature != artifact.Source+".sig" || !isLowerHex(strings.ToLower(artifact.SigningKeyFingerprint), 40) ||
				artifact.UpstreamDigestAlgorithm != "" || artifact.UpstreamDigest != "" {
				t.Fatalf("invalid AWS publisher provenance: %#v", artifact)
			}
		default:
			if artifact.UpstreamDigestAlgorithm != "" || artifact.UpstreamDigest != "" || artifact.Signature != "" || artifact.SigningKeyFingerprint != "" {
				t.Fatalf("unexpected publisher provenance on %q", artifact.Name)
			}
		}
		providerVersions[artifact.Name] = artifact.Version
		artifactNames = append(artifactNames, artifact.Name)
	}
	sort.Strings(artifactNames)
	if strings.Join(artifactNames, ",") != "aws-cli-v2,direnv,dotnet-sdk,fzf,go,kotlin,node,pnpm,starship,temurin-jdk,uv" {
		t.Fatalf("locked artifacts = %v", artifactNames)
	}

	manager := lock.Plugins.Manager
	if manager.Name != "antidote" || manager.Version != "2.3.0" || !isLowerHex(manager.Revision, 40) || !isLowerHex(manager.SHA256, 64) || !strings.Contains(manager.Source, manager.Revision) {
		t.Fatalf("invalid plugin manager lock: %#v", manager)
	}
	if !strings.Contains(recipe, "--checksum=sha256:"+manager.SHA256+" "+manager.Source) {
		t.Fatal("Containerfile does not checksum locked Antidote source")
	}

	pluginsData, err := os.ReadFile(filepath.Join(imageRoot, "shell", "zsh_plugins.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pluginLines := strings.Split(strings.TrimSpace(string(pluginsData)), "\n")
	expectedRepositories := []string{
		"zsh-users/zsh-completions",
		"Aloxaf/fzf-tab",
		"zsh-users/zsh-history-substring-search",
		"zsh-users/zsh-autosuggestions",
		"zsh-users/zsh-syntax-highlighting",
	}
	expectedPhases := []string{
		"pre-compinit-fpath",
		"post-compinit",
		"post-compinit",
		"post-compinit",
		"post-compinit-last",
	}
	if len(lock.Plugins.Entries) != len(expectedRepositories) || len(pluginLines) != len(expectedRepositories) {
		t.Fatalf("locked plugins=%d list lines=%d", len(lock.Plugins.Entries), len(pluginLines))
	}
	for i, plugin := range lock.Plugins.Entries {
		if plugin.Repository != expectedRepositories[i] || plugin.Phase != expectedPhases[i] || plugin.Order != i+1 {
			t.Fatalf("plugin %d violates set/order/phase contract: %#v", i, plugin)
		}
		if !isLowerHex(plugin.Revision, 40) || !isLowerHex(plugin.SHA256, 64) || !strings.Contains(plugin.Source, plugin.Revision) {
			t.Fatalf("plugin %q is not fully pinned", plugin.Repository)
		}
		if !strings.Contains(recipe, "--checksum=sha256:"+plugin.SHA256+" "+plugin.Source) {
			t.Fatalf("Containerfile does not checksum plugin %q", plugin.Repository)
		}
		expectedLine := plugin.Repository + " pin:" + plugin.Revision
		if i == 0 {
			expectedLine = plugin.Repository + " path:src kind:fpath pin:" + plugin.Revision
		}
		if pluginLines[i] != expectedLine {
			t.Fatalf("zsh_plugins.txt line %d = %q, want %q", i+1, pluginLines[i], expectedLine)
		}
	}
	for _, forbidden := range []string{"oh-my-zsh", "nvm", "pyenv"} {
		if strings.Contains(strings.ToLower(string(pluginsData)), forbidden) {
			t.Fatalf("forbidden plugin family %q is present", forbidden)
		}
	}

	expectedExecutables := map[string]string{
		"aws":           "/usr/local/bin/aws",
		"aws_completer": "/usr/local/bin/aws_completer",
		"bat":           "/usr/local/bin/bat",
		"c++":           "/usr/bin/c++",
		"cc":            "/usr/bin/cc",
		"curl":          "/usr/bin/curl",
		"direnv":        "/usr/local/bin/direnv",
		"dnx":           "/opt/dotnet/dnx",
		"dotnet":        "/opt/dotnet/dotnet",
		"fzf":           "/usr/local/bin/fzf",
		"git":           "/usr/bin/git",
		"go":            "/opt/go/bin/go",
		"java":          "/opt/java/bin/java",
		"javac":         "/opt/java/bin/javac",
		"jq":            "/usr/bin/jq",
		"kotlin":        "/opt/kotlin/bin/kotlin",
		"kotlinc":       "/opt/kotlin/bin/kotlinc",
		"less":          "/usr/bin/less",
		"make":          "/usr/bin/make",
		"node":          "/opt/node/bin/node",
		"npm":           "/opt/node/bin/npm",
		"pip":           "/usr/local/bin/pip",
		"pnpm":          "/usr/local/bin/pnpm",
		"python":        "/usr/local/bin/python",
		"python3":       "/usr/bin/python3",
		"rg":            "/usr/bin/rg",
		"ssh":           "/usr/bin/ssh",
		"starship":      "/usr/local/bin/starship",
		"tmux":          "/usr/bin/tmux",
		"unzip":         "/usr/bin/unzip",
		"uv":            "/usr/local/bin/uv",
		"uvx":           "/usr/local/bin/uvx",
		"zip":           "/usr/bin/zip",
		"zsh":           "/bin/zsh",
	}
	versionOverrides := map[string]string{
		"npm":     "11.17.0",
		"pip":     "25.1.1",
		"python":  "3.14.3",
		"python3": "3.14.3",
	}
	toolNames := make([]string, 0, len(lock.Tools))
	for _, tool := range lock.Tools {
		providerVersion, found := providerVersions[tool.Provider]
		if tool.Name == "" || tool.Version == "" || tool.Executable == "" || !found {
			t.Fatalf("tool has incomplete or unknown provider contract: %#v", tool)
		}
		expectedVersion := providerVersion
		if override := versionOverrides[tool.Name]; override != "" {
			expectedVersion = override
		}
		if tool.Version != expectedVersion {
			t.Fatalf("tool %q version = %q, want %q", tool.Name, tool.Version, expectedVersion)
		}
		if expectedExecutables[tool.Name] != tool.Executable {
			t.Fatalf("tool %q executable = %q, want %q", tool.Name, tool.Executable, expectedExecutables[tool.Name])
		}
		toolNames = append(toolNames, tool.Name)
	}
	sort.Strings(toolNames)
	const expectedTools = "aws,aws_completer,bat,c++,cc,curl,direnv,dnx,dotnet,fzf,git,go,java,javac,jq,kotlin,kotlinc,less,make,node,npm,pip,pnpm,python,python3,rg,ssh,starship,tmux,unzip,uv,uvx,zip,zsh"
	if strings.Join(toolNames, ",") != expectedTools {
		t.Fatalf("locked tools = %v", toolNames)
	}

	const expectedPath = "/opt/dotnet:/opt/dotnet/tools:/opt/kotlin/bin:/opt/node/bin:/opt/go/bin:/opt/java/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if lock.Environment.Shell != "/bin/zsh" ||
		lock.Environment.ZDOTDir != "/usr/local/share/dsx/shell" ||
		lock.Environment.Path != expectedPath ||
		lock.Environment.JavaHome != "/opt/java" ||
		lock.Environment.DotnetRoot != "/opt/dotnet" ||
		lock.Environment.KotlinHome != "/opt/kotlin" ||
		lock.Environment.StarshipConfig != "/usr/local/share/dsx/shell/starship.toml" {
		t.Fatalf("invalid image environment contract: %#v", lock.Environment)
	}
	for _, fragment := range []string{
		"ENV HOME=/home/dsx",
		"USER=dsx",
		"LOGNAME=dsx",
		"SHELL=/bin/zsh",
		"ZDOTDIR=" + lock.Environment.ZDOTDir,
		"JAVA_HOME=" + lock.Environment.JavaHome,
		"DOTNET_ROOT=" + lock.Environment.DotnetRoot,
		"KOTLIN_HOME=" + lock.Environment.KotlinHome,
		"STARSHIP_CONFIG=" + lock.Environment.StarshipConfig,
		"PATH=" + lock.Environment.Path,
		"groupadd --gid 1000 dsx",
		"useradd --uid 1000 --gid 1000 --create-home --shell /bin/zsh dsx",
		"userdel --remove ubuntu",
		"install -d -o 1000 -g 1000 -m 0700 /run/dsx /workspace /home/dsx/.dsx/auth /home/dsx/.local/state/dsx /home/dsx/.cache /var/lib/dsx",
		"COPY --chmod=0440 sudoers-dsx /etc/sudoers.d/dsx",
		"visudo -cf /etc/sudoers.d/dsx",
		"ln -s /opt/pnpm/bin/pnpm.mjs /usr/local/bin/pnpm",
		"ln -s /usr/bin/python3 /usr/local/bin/python",
		"ln -s /usr/bin/pip3 /usr/local/bin/pip",
		"ln -s /usr/bin/batcat /usr/local/bin/bat",
		"COPY --chmod=0444 shell-toolchains.lock.json /usr/local/share/dsx/shell-toolchains.lock.json",
		"COPY --chmod=0555 shell/dsx.zsh /usr/local/share/dsx/shell/dsx.zsh",
		"COPY --chmod=0444 shell/zsh_plugins.txt shell/starship.toml /usr/local/share/dsx/shell/",
		"printf '%s\\n' 'source /usr/local/share/dsx/shell/dsx.zsh' > /usr/local/share/dsx/shell/.zshrc",
		"antidote bundle",
		"compinit -i -d " + lock.Generated.CompletionCache,
		"fzf --zsh > " + lock.Generated.FZFInit,
		"direnv hook zsh > " + lock.Generated.DirenvInit,
		"starship init zsh > " + lock.Generated.StarshipInit,
		"COPY --from=artifacts /opt/shell-src/antidote /opt/antidote",
		lock.Generated.PluginsPre,
		lock.Generated.PluginsPost,
	} {
		if !strings.Contains(recipe, fragment) {
			t.Fatalf("Containerfile missing shell contract %q", fragment)
		}
	}
	if lock.Generated.PluginsPre != "/usr/local/share/dsx/shell/plugins-pre.zsh" ||
		lock.Generated.PluginsPost != "/usr/local/share/dsx/shell/plugins-post.zsh" ||
		lock.Generated.CompletionCache != "/usr/local/share/dsx/shell/zcompdump" ||
		lock.Generated.FZFInit != "/usr/local/share/dsx/shell/fzf-init.zsh" ||
		lock.Generated.DirenvInit != "/usr/local/share/dsx/shell/direnv-init.zsh" ||
		lock.Generated.StarshipInit != "/usr/local/share/dsx/shell/starship-init.zsh" {
		t.Fatalf("invalid generated shell paths: %#v", lock.Generated)
	}
	chmodAt := strings.Index(recipe, "&& chmod 0444 "+lock.Generated.PluginsPre)
	chmodBlock := ""
	if chmodAt >= 0 {
		if chmodEnd := strings.Index(recipe[chmodAt:], "\n\n"); chmodEnd >= 0 {
			chmodBlock = recipe[chmodAt : chmodAt+chmodEnd]
		}
	}
	userAt := strings.Index(recipe, "USER 1000:1000")
	if chmodBlock == "" || userAt < chmodAt || !strings.Contains(chmodBlock, lock.Generated.DirenvInit) {
		t.Fatal("build must make the root-generated static direnv init read-only before switching users")
	}

	defaultsData, err := os.ReadFile(filepath.Join(imageRoot, "shell", "dsx.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	defaults := string(defaultsData)
	pluginsPreAt := strings.Index(defaults, `source "${_DSX_SHELL_ROOT}/plugins-pre.zsh"`)
	compinitAt := strings.Index(defaults, `compinit -C -d "${_DSX_SHELL_ROOT}/zcompdump"`)
	fzfInitAt := strings.Index(defaults, `source "${_DSX_SHELL_ROOT}/fzf-init.zsh"`)
	pluginsPostAt := strings.Index(defaults, `source "${_DSX_SHELL_ROOT}/plugins-post.zsh"`)
	direnvInitAt := strings.Index(defaults, `source "${_DSX_SHELL_ROOT}/direnv-init.zsh"`)
	starshipInitAt := strings.Index(defaults, `source "${_DSX_SHELL_ROOT}/starship-init.zsh"`)
	if pluginsPreAt < 0 || compinitAt <= pluginsPreAt || fzfInitAt <= compinitAt ||
		pluginsPostAt <= fzfInitAt || direnvInitAt <= pluginsPostAt || starshipInitAt <= direnvInitAt {
		t.Fatal("managed shell startup order must be pre plugins, cached compinit, static fzf init, post plugins, static direnv init, then static Starship init")
	}
	for _, forbiddenRuntimeForm := range []string{
		"antidote bundle",
		"antidote update",
		"fzf --zsh",
		"direnv hook zsh",
		"starship init zsh",
		"curl -fsSL",
		"curl -sSL",
		"wget ",
		"git clone",
	} {
		if strings.Contains(defaults, forbiddenRuntimeForm) {
			t.Fatalf("managed shell startup contains runtime update, download, or generation form %q", forbiddenRuntimeForm)
		}
	}

	for _, forbidden := range []string{"curl -fsSL", "curl -sSL", "| sh", "| bash", "antidote update", "git clone", "apt-get install awscli", "pip install awscli", "dotnet-install.sh", "apt-get install dotnet-sdk", "gradle", "maven", "kotlin-native"} {
		if strings.Contains(strings.ToLower(recipe), forbidden) {
			t.Fatalf("Containerfile contains unpinned or forbidden command %q", forbidden)
		}
	}
	normalizedRecipe := strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(strings.ToLower(recipe))
	for _, unsafeAPT := range []string{
		"acquire::https::verify-peer=false",
		"acquire::https::verify-host=false",
		"trusted=yes",
		"allow-unauthenticated",
		"http://snapshot.ubuntu.com",
	} {
		if strings.Contains(normalizedRecipe, unsafeAPT) {
			t.Fatalf("Containerfile contains unsafe apt form %q", unsafeAPT)
		}
	}
	for remaining := canonicalRecipe; ; {
		installAt := strings.Index(remaining, "apt-get install")
		if installAt < 0 {
			break
		}
		remaining = remaining[installAt:]
		commandEnd := strings.Index(remaining, " &&")
		if commandEnd < 0 {
			commandEnd = len(remaining)
		}
		fields := strings.Fields(remaining[:commandEnd])
		for _, field := range fields[2:] {
			if !strings.HasPrefix(field, "-") && !strings.Contains(field, "=") {
				t.Fatalf("Containerfile contains unpinned apt install package %q", field)
			}
		}
		remaining = remaining[commandEnd:]
	}
	if strings.Contains(recipe, "rm -rf /opt/antidote") {
		t.Fatal("Containerfile removes pinned Antidote required by managed Zsh")
	}
	if strings.Count(recipe, "LABEL io.dsx.harness-lock.sha256=\"0dbd480a8a9c325430c4237476e9feac7556857ef76c6432f965f3459a7b2650\"") != 1 {
		t.Fatalf("harness attestation label changed: %q", recipe)
	}
}
