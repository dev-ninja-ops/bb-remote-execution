//go:build !linux
// +build !linux

package main

import "fmt"

// ensureCgroupParent is a no-op stub on non-Linux platforms; setting
// cgroup_parent_path in the runner config on a non-Linux runner is an
// operator error.
func ensureCgroupParent(parentPath string) error {
	return fmt.Errorf("cgroup_parent_path is set to %q, but cgroup placement is only supported on Linux", parentPath)
}
