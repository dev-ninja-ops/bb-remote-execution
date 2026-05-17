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

	"github.com/buildbarn/bb-remote-execution/pkg/proto/resourceusage"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/google/uuid"
)

const cgroupV2Mountpoint = "/sys/fs/cgroup"

// cgroupHandle wraps the per-action cgroup created by setupCgroup. It
// gives the caller access to live stats while the action is running
// and a single Close() that tears the cgroup down at the end.
type cgroupHandle struct {
	mgr    *cgroup2.Manager
	dir    string
	fd     *os.File
	limits map[string]string // original request limits, used for the requested_* fields
}

// Stats returns a snapshot of the cgroup right now. Safe to call from
// any goroutine while the action is in flight. Returns a fully-populated
// CGroupResourceUsage including the limits we applied.
func (h *cgroupHandle) Stats() (*resourceusage.CGroupResourceUsage, error) {
	return readCgroupStats(h.mgr, h.dir, h.limits)
}

// Close kills any survivors in the cgroup and removes the cgroup
// directory. Idempotent. Safe to call after Stats().
func (h *cgroupHandle) Close() {
	if h == nil {
		return
	}
	if h.fd != nil {
		_ = h.fd.Close()
		h.fd = nil
	}
	// Kill descendants that out-lived the direct child so Delete()
	// doesn't fail with EBUSY.
	_ = h.mgr.Kill()
	_ = h.mgr.Delete()
}

// setupCgroup creates a per-action cgroup v2 sub-cgroup beneath
// r.cgroupParentPath (e.g. "/sys/fs/cgroup/bb-actions/run-<uuid>"),
// converts the request's resource_limits into cgroup writes, opens a
// file descriptor on the new cgroup directory, and arranges for the
// child process to be cloned directly into the cgroup via Linux's
// CLONE_INTO_CGROUP (clone3) — which Go exposes through
// SysProcAttr.UseCgroupFD / CgroupFD on Linux 5.7+ kernels.
//
// The cgroup is named "run-<runID>" when runID is non-empty (so
// callers like bb_worker can later look it up by id via
// GetLiveCgroupStats); otherwise a UUID is generated locally for
// back-compat with older callers.
//
// Returns (nil, nil) when cgroup placement is disabled (parent unset,
// or limits empty). Caller must Close() the returned handle when done.
func (r *localRunner) setupCgroup(cmd *exec.Cmd, limits map[string]string, runID string) (*cgroupHandle, error) {
	if r.cgroupParentPath == "" || len(limits) == 0 {
		return nil, nil
	}
	if !strings.HasPrefix(r.cgroupParentPath, cgroupV2Mountpoint+"/") && r.cgroupParentPath != cgroupV2Mountpoint {
		return nil, fmt.Errorf("cgroup_parent_path %q must live under %s", r.cgroupParentPath, cgroupV2Mountpoint)
	}

	res, err := buildCgroupResources(limits)
	if err != nil {
		return nil, err
	}

	groupRel, err := filepath.Rel(cgroupV2Mountpoint, r.cgroupParentPath)
	if err != nil {
		return nil, fmt.Errorf("compute relative cgroup path: %w", err)
	}
	name := runID
	if name == "" {
		name = uuid.New().String()
	}
	// containerd/cgroups expects the group argument to start with "/".
	childGroup := "/" + filepath.Join(groupRel, "run-"+name)

	mgr, err := cgroup2.NewManager(cgroupV2Mountpoint, childGroup, res)
	if err != nil {
		return nil, fmt.Errorf("create cgroup %s: %w", childGroup, err)
	}

	cgroupDir := filepath.Join(cgroupV2Mountpoint, childGroup)
	fd, err := os.Open(cgroupDir)
	if err != nil {
		_ = mgr.Delete()
		return nil, fmt.Errorf("open cgroup dir %s: %w", cgroupDir, err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())

	return &cgroupHandle{mgr: mgr, dir: cgroupDir, fd: fd, limits: limits}, nil
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

// readCgroupStats returns a CGroupResourceUsage snapshot of cgroupDir
// at the moment of the call. Pulls cpu.stat / memory.events from
// containerd's Stat(); reads memory.current and memory.peak directly
// because they're not in the parsed Metrics struct.
func readCgroupStats(mgr *cgroup2.Manager, cgroupDir string, limits map[string]string) (*resourceusage.CGroupResourceUsage, error) {
	out := &resourceusage.CGroupResourceUsage{}

	// Limits: echoed back so the message is self-describing.
	if v, ok := limits["cpu"]; ok {
		if millicores, err := strconv.ParseUint(v, 10, 32); err == nil && millicores > 0 {
			out.RequestedCpuMax = fmt.Sprintf("%d 100000", millicores*100)
		}
	}
	if v, ok := limits["memory"]; ok {
		if bytes, err := strconv.ParseUint(v, 10, 64); err == nil {
			out.RequestedMemoryMax = bytes
		}
	}
	if v, ok := limits["gpu_uuids"]; ok && v != "" {
		out.GpuUuids = strings.Split(v, ",")
	}

	// Parsed counters: cpu.stat and memory.events come back as
	// structured fields from containerd's Stat().
	if metrics, err := mgr.Stat(); err == nil {
		if metrics.CPU != nil {
			out.CpuUsageUsec = metrics.CPU.UsageUsec
			out.CpuUserUsec = metrics.CPU.UserUsec
			out.CpuSystemUsec = metrics.CPU.SystemUsec
			out.NrPeriods = metrics.CPU.NrPeriods
			out.NrThrottled = metrics.CPU.NrThrottled
			out.ThrottledUsec = metrics.CPU.ThrottledUsec
		}
		if metrics.MemoryEvents != nil {
			out.OomKillCount = metrics.MemoryEvents.OomKill
		}
	}

	// memory.current and memory.peak aren't exposed by Stat(), read
	// them directly. Both are single-line uint64 in bytes.
	if b, err := os.ReadFile(filepath.Join(cgroupDir, "memory.current")); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			out.CurrentMemoryBytes = n
		}
	}
	if b, err := os.ReadFile(filepath.Join(cgroupDir, "memory.peak")); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			out.PeakMemoryBytes = n
		}
	}
	return out, nil
}
