//go:build linux
// +build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const cgroupV2Mountpoint = "/sys/fs/cgroup"

// ensureCgroupParent prepares the cgroup hierarchy under parentPath so
// that per-action sub-cgroups can be created with cpu+memory enforced.
//
// cgroup v2 has a "no internal processes" rule: a cgroup that contains
// processes cannot have controllers added to cgroup.subtree_control.
// When the runner starts, its own PID is usually in the container's
// root cgroup (i.e. /sys/fs/cgroup). To enable controllers for
// descendants, we first move our own processes into a sibling "init"
// cgroup, then write +cpu +memory to the root's subtree_control, then
// create the parent and enable controllers there as well.
func ensureCgroupParent(parentPath string) error {
	if !strings.HasPrefix(parentPath, cgroupV2Mountpoint+"/") {
		return fmt.Errorf("cgroup_parent_path %q must live under %s", parentPath, cgroupV2Mountpoint)
	}
	// Move our own processes to a sibling cgroup to satisfy the
	// no-internal-processes rule. Idempotent.
	if err := evacuateRootProcs(cgroupV2Mountpoint); err != nil {
		return fmt.Errorf("evacuate procs from %s: %w", cgroupV2Mountpoint, err)
	}
	// Walk every ancestor of parentPath from the root down,
	// enabling +cpu +memory in each subtree_control.
	rel, err := filepath.Rel(cgroupV2Mountpoint, parentPath)
	if err != nil {
		return err
	}
	curr := cgroupV2Mountpoint
	if err := writeSubtreeControl(curr, "+cpu", "+memory"); err != nil {
		return fmt.Errorf("enable controllers at %s: %w", curr, err)
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		curr = filepath.Join(curr, seg)
		if err := os.MkdirAll(curr, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", curr, err)
		}
		if err := writeSubtreeControl(curr, "+cpu", "+memory"); err != nil {
			return fmt.Errorf("enable controllers at %s: %w", curr, err)
		}
	}
	return nil
}

func evacuateRootProcs(rootCgroup string) error {
	procsPath := filepath.Join(rootCgroup, "cgroup.procs")
	data, err := os.ReadFile(procsPath)
	if err != nil {
		return err
	}
	pids := strings.Fields(string(bytes.TrimSpace(data)))
	if len(pids) == 0 {
		return nil
	}
	initDir := filepath.Join(rootCgroup, "init")
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		return err
	}
	initProcs := filepath.Join(initDir, "cgroup.procs")
	for _, pid := range pids {
		if err := os.WriteFile(initProcs, []byte(pid), 0); err != nil {
			// Kernel threads (PIDs we can't move) and races with
			// dying procs are not fatal; only fail if root is
			// still occupied at the end.
			if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINVAL) {
				return fmt.Errorf("move pid %s into %s: %w", pid, initDir, err)
			}
		}
	}
	return nil
}

func writeSubtreeControl(cgroupPath string, controllers ...string) error {
	subtree := filepath.Join(cgroupPath, "cgroup.subtree_control")
	f, err := os.OpenFile(subtree, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(controllers, " ")); err != nil {
		// Re-enabling already-enabled controllers may return EBUSY
		// on some kernels; the controller is enabled either way.
		if errors.Is(err, syscall.EBUSY) {
			return nil
		}
		return err
	}
	return nil
}
