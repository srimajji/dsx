// Package agentimage exposes the DSX-owned standard agent image build inputs.
package agentimage

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
)

const (
	BuildContext = "."
	BuildFile    = "Containerfile"
	assetMode    = os.FileMode(0o600)
)

//go:embed Containerfile harnesses.lock.json
var assets embed.FS

// InputDigest returns the authority digest for the exact context Materialize writes.
func InputDigest() string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("dsx.build-input/v1\x00"))
	hashAsset(digest, BuildFile)
	hashAsset(digest, "harnesses.lock.json")
	return hex.EncodeToString(digest.Sum(nil))
}

// Materialize writes the embedded standard image context into an empty private directory.
func Materialize(root string) error {
	for _, name := range []string{BuildFile, "harnesses.lock.json"} {
		content, err := assets.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read embedded standard image asset %q: %w", name, err)
		}
		file, err := os.OpenFile(root+string(os.PathSeparator)+name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, assetMode)
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

func hashAsset(digest hash.Hash, name string) {
	content, err := assets.ReadFile(name)
	if err != nil {
		panic("embedded standard image asset is unavailable: " + err.Error())
	}
	_, _ = fmt.Fprintf(digest, "%d:%s:%#o:%d:", len(name), name, uint32(assetMode), len(content))
	_, _ = digest.Write(content)
	_, _ = digest.Write([]byte{0})
}
