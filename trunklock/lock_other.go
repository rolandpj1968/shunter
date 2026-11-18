//go:build !unix

package trunklock

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("trunklock: needs flock, on a unix system")

func lockNB(*os.File, bool) (bool, error)     { return false, errUnsupported }
func unlock(*os.File)                         {}
func samePath(*os.File, string) (bool, error) { return false, errUnsupported }
func alive(int) bool                          { return true }
