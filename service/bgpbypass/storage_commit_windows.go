//go:build windows

package bgpbypass

import "golang.org/x/sys/windows"

// Windows does not support POSIX directory fsync. The temporary file has
// already been flushed; request a write-through replacement on the same volume.
func commitStateFile(name, path string) error {
	from, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
