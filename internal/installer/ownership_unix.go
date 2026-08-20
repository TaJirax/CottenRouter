//go:build !windows

package installer

import (
	"errors"
	"os"
	"syscall"
)

// preserveFileOwnership keeps the owner/group of an existing destination when
// atomicWrite replaces it. This is required for the root:cottenrouter config
// contract and for backend configs owned by native service accounts.
func preserveFileOwnership(stagedPath, destination string) error {
	info, err := os.Stat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(stagedPath, int(stat.Uid), int(stat.Gid))
}
