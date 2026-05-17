//go:build linux
// +build linux

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/google/uuid"
)

const cgroupV2Mountpoint = "/sys/fs/cgroup"

// setupCgroup creates a per-action cgroup v2 sub-cgroup beneath
// r.cgroupParentPath (e.g. "/sys/fs/cgroup/bb-actions/run-<uuid>"),
// converts the request's resource_limits into cgroup writes, opens a
// file descriptor on the new cgroup directory, and arranges for the
// child process to be cloned directly into the cgroup via Linux's
// CLONE_INTO_CGROUP (clone3) — which Go exposes through
// SysProcAttr.UseCgroupFD / CgroupFD on Linux 5.7+ kernels.
//
// Returns a cleanup function that closes the cgroup fd, kills any
// surviving processes in the cgroup, and removes the cgroup directory.
// On error, the partial state is cleaned up before returning.
func (r *localRunner) setupCgroup(cmd *exec.Cmd, limits map[string]string) (func(), error) {
	noop := func() {}
	if r.cgroupParentPath == "" || len(limits) == 0 {
		return noop, nil
	}
	if !strings.HasPrefix(r.cgroupParentPath, cgroupV2Mountpoint+"/") && r.cgroupParentPath != cgroupV2Mountpoint {
		return noop, fmt.Errorf("cgroup_parent_path %q must live under %s", r.cgroupParentPath, cgroupV2Mountpoint)
	}

	res, err := buildCgroupResources(limits)
	if err != nil {
		return noop, err
	}

	groupRel, err := filepath.Rel(cgroupV2Mountpoint, r.cgroupParentPath)
	if err != nil {
		return noop, fmt.Errorf("compute relative cgroup path: %w", err)
	}
	// containerd/cgroups expects the group argument to start with "/".
	childGroup := "/" + filepath.Join(groupRel, "run-"+uuid.New().String())

	mgr, err := cgroup2.NewManager(cgroupV2Mountpoint, childGroup, res)
	if err != nil {
		return noop, fmt.Errorf("create cgroup %s: %w", childGroup, err)
	}

	cgroupDir := filepath.Join(cgroupV2Mountpoint, childGroup)
	fd, err := os.Open(cgroupDir)
	if err != nil {
		_ = mgr.Delete()
		return noop, fmt.Errorf("open cgroup dir %s: %w", cgroupDir, err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())

	cleanup := func() {
		_ = fd.Close()
		// Kill any descendants that out-lived the direct child
		// (double-forkers etc.) so Delete() does not fail with EBUSY.
		_ = mgr.Kill()
		_ = mgr.Delete()
	}
	return cleanup, nil
}

// buildCgroupResources converts the resource_limits map (as sent by
// the build executor) into a cgroup2.Resources value.
func buildCgroupResources(limits map[string]string) (*cgroup2.Resources, error) {
	res := &cgroup2.Resources{}
	if v, ok := limits["cpu"]; ok {
		millicores, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu %q: %w", v, err)
		}
		if millicores > 0 {
			period := uint64(100_000)
			quota := int64(millicores) * 100 // millicores -> microseconds per 100ms period
			res.CPU = &cgroup2.CPU{
				Max: cgroup2.NewCPUMax(&quota, &period),
			}
		}
	}
	if v, ok := limits["memory"]; ok {
		bytes, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid memory %q: %w", v, err)
		}
		if bytes > 0 {
			res.Memory = &cgroup2.Memory{Max: &bytes}
		}
	}
	// gpu_uuids are honored via NVIDIA_VISIBLE_DEVICES in the env;
	// cgroup-level device gating on cgroup v2 requires an eBPF
	// program which is deferred to a follow-up.
	return res, nil
}
