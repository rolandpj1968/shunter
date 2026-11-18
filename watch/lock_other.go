//go:build !unix

package watch

import "errors"

func lockHeld(string) (bool, error) { return false, errors.New("watch: needs flock, on a unix system") }
