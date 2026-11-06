//go:build !windows

package ipc

import (
	"errors"
	"os"
)

func createPrivateTemp(directory, pattern string) (*os.File, error) {
	return os.CreateTemp(directory, pattern)
}

func replacePrivate(source, target string, _ os.FileInfo) error {
	return os.Rename(source, target)
}

func removeOwned(path string, published os.FileInfo) error {
	if published == nil {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(info, published) {
		return os.Remove(path)
	}
	return nil
}
