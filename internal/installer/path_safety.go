package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// rejectSymlinkComponents fails closed when a managed path or any existing
// ancestor is a symlink. Install and purge run as root, so following an
// attacker-controlled link is never an acceptable convenience.
func rejectSymlinkComponents(path string) error {
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	for current := clean; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlinked managed path component %q", current)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}
