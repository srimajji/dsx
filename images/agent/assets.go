// Package agentimage exposes the DSX-owned standard agent image build inputs.
package agentimage

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

const (
	BuildContext  = "."
	BuildFile     = "Containerfile"
	assetMode     = os.FileMode(0o600)
	directoryMode = os.FileMode(0o700)
)

// Keep this in filepath.WalkDir order so InputDigest matches the staged context digest.
var standardImageAssetNames = [...]string{
	BuildFile,
	"harnesses.lock.json",
	"shell/dsx.zsh",
	"shell/starship.toml",
	"shell/zsh_plugins.txt",
	"shell-toolchains.lock.json",
	"sudoers-dsx",
}

//go:embed Containerfile harnesses.lock.json shell-toolchains.lock.json sudoers-dsx shell/dsx.zsh shell/zsh_plugins.txt shell/starship.toml
var assets embed.FS

// InputDigest returns the authority digest for the exact context Materialize writes.
func InputDigest() string {
	return inputDigest(assets)
}

func inputDigest(source fs.FS) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("dsx.build-input/v1\x00"))
	for _, name := range standardImageAssetNames {
		hashAsset(digest, source, name)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Materialize writes the embedded standard image context into an empty private directory.
func Materialize(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read standard image asset root %q: %w", root, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("materialize standard image assets in %q: root is not empty", root)
	}

	createdDirectories := make(map[string]struct{})
	for _, name := range standardImageAssetNames {
		if err := createAssetDirectory(root, path.Dir(name), createdDirectories); err != nil {
			return err
		}
	}

	for _, name := range standardImageAssetNames {
		content, err := assets.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read embedded standard image asset %q: %w", name, err)
		}
		file, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, assetMode)
		if err != nil {
			return fmt.Errorf("create standard image asset %q: %w", name, err)
		}
		written, writeErr := file.Write(content)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("write standard image asset %q: %w", name, writeErr)
		}
		if written != len(content) {
			return fmt.Errorf("write standard image asset %q: short write", name)
		}
		if closeErr != nil {
			return fmt.Errorf("close standard image asset %q: %w", name, closeErr)
		}
	}
	return nil
}

func createAssetDirectory(root, name string, created map[string]struct{}) error {
	if name == "." {
		return nil
	}
	if !fs.ValidPath(name) {
		return fmt.Errorf("create standard image asset directory %q: invalid embedded path", name)
	}
	if _, ok := created[name]; ok {
		return nil
	}
	if err := createAssetDirectory(root, path.Dir(name), created); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(name)), directoryMode); err != nil {
		return fmt.Errorf("create standard image asset directory %q: %w", name, err)
	}
	created[name] = struct{}{}
	return nil
}

func hashAsset(digest hash.Hash, source fs.FS, name string) {
	content, err := fs.ReadFile(source, name)
	if err != nil {
		panic("embedded standard image asset is unavailable: " + err.Error())
	}
	_, _ = fmt.Fprintf(digest, "%d:%s:%#o:%d:", len(name), name, uint32(assetMode), len(content))
	_, _ = digest.Write(content)
	_, _ = digest.Write([]byte{0})
}
