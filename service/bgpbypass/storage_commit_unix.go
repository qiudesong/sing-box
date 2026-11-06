//go:build !windows

package bgpbypass

import (
	"os"
	"path/filepath"

	E "github.com/sagernet/sing/common/exceptions"
)

func commitStateFile(name, path string) error {
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return E.Cause(err, "sync state directory")
	}
	return nil
}
