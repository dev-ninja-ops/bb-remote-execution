//go:build !linux
// +build !linux

package runner

import "os/exec"

// setupCgroup is a no-op on non-Linux platforms; cgroup v2 placement
// is a Linux-only feature.
func (r *localRunner) setupCgroup(cmd *exec.Cmd, limits map[string]string) (func(), error) {
	return func() {}, nil
}
