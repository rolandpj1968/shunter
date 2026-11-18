//go:build unix

package watch

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockHeld reports whether a process holds the store's lock. It takes a
// shared lock for a moment to find out: a daemon starting in that moment
// fails to take its own and is restarted by its supervisor.
func lockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("watch: the store's lock: %w", err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("watch: the store's lock: %w", err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
