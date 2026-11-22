//go:build !windows

package work

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockWorkspace(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, lockedError(err)
	}
	return file, nil
}
