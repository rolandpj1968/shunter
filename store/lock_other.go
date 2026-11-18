//go:build !unix

package store

import (
	"errors"
	"os"
)

func lockFile(string) (*os.File, error) {
	return nil, errors.New("store: the single-writer lock needs flock, which this platform lacks")
}

func unlock(*os.File) {}
