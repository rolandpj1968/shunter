//go:build !unix

package hook

import "os/exec"

func setGroup(*exec.Cmd) {}
