package agentimage

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
)

func TestMaterializeWritesEveryEmbeddedAsset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "context")
	if err := os.Mkdir(root, directoryMode); err != nil {
		t.Fatal(err)
	}

	if err := Materialize(root); err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}

	for _, name := range standardImageAssetNames {
		want, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded asset %q: %v", name, err)
		}
		outputPath := filepath.Join(root, filepath.FromSlash(name))
		got, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("read materialized asset %q: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("materialized asset %q differs from embedded content", name)
		}
		info, err := os.Stat(outputPath)
		if err != nil {
			t.Fatalf("stat materialized asset %q: %v", name, err)
		}
		if gotMode := info.Mode().Perm(); gotMode != assetMode {
			t.Errorf("materialized asset %q mode = %#o, want %#o", name, gotMode, assetMode)
		}
	}

	shellInfo, err := os.Stat(filepath.Join(root, "shell"))
	if err != nil {
		t.Fatalf("stat materialized shell directory: %v", err)
	}
	if !shellInfo.IsDir() {
		t.Fatal("materialized shell path is not a directory")
	}
	if gotMode := shellInfo.Mode().Perm(); gotMode != directoryMode {
		t.Errorf("materialized shell directory mode = %#o, want %#o", gotMode, directoryMode)
	}

	var gotPaths []string
	if err := filepath.WalkDir(root, func(outputPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, outputPath)
		if err != nil {
			return err
		}
		gotPaths = append(gotPaths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatalf("walk materialized assets: %v", err)
	}
	wantPaths := []string{
		".",
		BuildFile,
		"harnesses.lock.json",
		"shell",
		"shell/dsx.zsh",
		"shell/starship.toml",
		"shell/zsh_plugins.txt",
		"shell-toolchains.lock.json",
	}
	slices.Sort(gotPaths)
	slices.Sort(wantPaths)
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("materialized paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestMaterializeRejectsNonEmptyRootWithoutChanges(t *testing.T) {
	tests := []struct {
		name     string
		seedPath string
	}{
		{name: "unrelated entry", seedPath: "sentinel"},
		{name: "top-level asset collision", seedPath: BuildFile},
		{name: "nested asset collision", seedPath: "shell/dsx.zsh"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "context")
			if err := os.Mkdir(root, directoryMode); err != nil {
				t.Fatal(err)
			}
			seedPath := filepath.Join(root, filepath.FromSlash(test.seedPath))
			if err := os.MkdirAll(filepath.Dir(seedPath), directoryMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(seedPath, []byte("preserve me"), assetMode); err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, root)

			if err := Materialize(root); err == nil {
				t.Fatal("Materialize() error = nil, want non-empty root error")
			}

			after := snapshotTree(t, root)
			if !reflect.DeepEqual(after, before) {
				t.Errorf("root changed after failed materialization: before = %#v, after = %#v", before, after)
			}
		})
	}
}

func TestInputDigestCoversAuthoritativeAssetList(t *testing.T) {
	wantNames := []string{
		BuildFile,
		"harnesses.lock.json",
		"shell/dsx.zsh",
		"shell/starship.toml",
		"shell/zsh_plugins.txt",
		"shell-toolchains.lock.json",
	}
	if !slices.Equal(standardImageAssetNames[:], wantNames) {
		t.Fatalf("standardImageAssetNames = %q, want %q", standardImageAssetNames, wantNames)
	}

	baseline := make(fstest.MapFS, len(standardImageAssetNames))
	for _, name := range standardImageAssetNames {
		content, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded asset %q: %v", name, err)
		}
		baseline[name] = &fstest.MapFile{Data: bytes.Clone(content), Mode: assetMode}
	}
	wantDigest := InputDigest()
	if got := inputDigest(baseline); got != wantDigest {
		t.Fatalf("inputDigest(shared list) = %q, want %q", got, wantDigest)
	}
	if got := InputDigest(); got != wantDigest {
		t.Fatalf("InputDigest() is not deterministic: got %q, want %q", got, wantDigest)
	}

	for _, changedName := range standardImageAssetNames {
		changed := make(fstest.MapFS, len(baseline))
		for name, file := range baseline {
			changed[name] = &fstest.MapFile{Data: bytes.Clone(file.Data), Mode: file.Mode}
		}
		changed[changedName].Data = append(changed[changedName].Data, 0)
		if got := inputDigest(changed); got == wantDigest {
			t.Errorf("digest did not change when listed asset %q changed", changedName)
		}
	}
}

type treeEntry struct {
	Mode    fs.FileMode
	Content []byte
}

func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	entries := make(map[string]treeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var content []byte
		if !entry.IsDir() {
			content, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[filepath.ToSlash(relative)] = treeEntry{Mode: info.Mode(), Content: content}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot tree: %v", err)
	}
	return entries
}
