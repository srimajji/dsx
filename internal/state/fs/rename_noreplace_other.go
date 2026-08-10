//go:build !darwin && !linux

package fs

import "os"

func renameNoReplace(source, destination string) error {
	if err := os.Link(source, destination); err != nil {
		return err
	}
	return os.Remove(source)
}
