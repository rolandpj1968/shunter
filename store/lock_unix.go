//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on path. The lock belongs
// to the open file, so a second Open in the same process is refused too, and
// the kernel releases it if the process dies.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: lock: %w", err)
	}
	// The holder's pid, for a human reading the directory. Not the lock.
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return f, nil
}

func unlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
