//go:build !linux
// +build !linux

package runner

import (
	"os/exec"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/resourceusage"
)

// cgroupHandle on non-Linux is a no-op type. setupCgroup never returns
// one; the rest of the code paths check for nil before touching it.
type cgroupHandle struct{}

func (h *cgroupHandle) Stats() (*resourceusage.CGroupResourceUsage, error) {
	return nil, nil
}
func (h *cgroupHandle) Close() {}

// setupCgroup is a no-op on non-Linux platforms; cgroup v2 placement
// is a Linux-only feature.
func (r *localRunner) setupCgroup(cmd *exec.Cmd, limits map[string]string, runID string) (*cgroupHandle, error) {
	return nil, nil
}
