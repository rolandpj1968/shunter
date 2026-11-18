//go:build unix

package trunklock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockNB tries to take the flock without waiting: exclusive or shared.
func lockNB(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, fmt.Errorf("trunklock: flock: %w", err)
}

func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// samePath reports whether the open file is the file at path now.
func samePath(f *os.File, path string) (bool, error) {
	a, err := f.Stat()
	if err != nil {
		return false, err
	}
	b, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(a, b), nil
}

// alive reports whether a process exists on this host.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
