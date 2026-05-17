# Buildbarn Remote Execution [![Build status](https://github.com/buildbarn/bb-remote-execution/workflows/main/badge.svg)](https://github.com/buildbarn/bb-remote-execution/actions) [![PkgGoDev](https://pkg.go.dev/badge/github.com/buildbarn/bb-remote-execution)](https://pkg.go.dev/github.com/buildbarn/bb-remote-execution) [![Go Report Card](https://goreportcard.com/badge/github.com/buildbarn/bb-remote-execution)](https://goreportcard.com/report/github.com/buildbarn/bb-remote-execution)

Translations: [Chinese](https://github.com/buildbarn/bb-remote-execution/blob/main/doc/zh_CN/README.md)

This repository provides tools that can be used in combination with
[the Buildbarn storage daemon](https://github.com/buildbarn/bb-storage)
to add support for remote execution, allowing you to create
[a build farm](https://en.wikipedia.org/wiki/Compile_farm) that can be
called into using tools such as [Bazel](https://bazel.build/),
[BuildStream](https://wiki.gnome.org/Projects/BuildStream) and
[recc](https://gitlab.com/bloomberg/recc).

This repository provides three programs:

- `bb_scheduler`: A service that receives requests from
  [`bb_storage`](https://github.com/buildbarn/bb-storage) to queue build
  actions that need to be run.
- `bb_worker`: A service that requests build actions from `bb_scheduler`
  and orchestrates their execution. This includes downloading the build
  action's input files and uploading its output files.
- `bb_runner`: A service that executes the command associated with the
  build action.

Most setups will run a single instance of `bb_scheduler` and a large
number of pairs of `bb_worker`/`bb_runner` processes. Older versions of
Buildbarn integrated the functionality of `bb_worker` and `bb_runner`
into a single process. These processes were decomposed to accomplish the
following:

- To make it possible to use privilege separation. Privilege separation
  is used to prevent build actions from overwriting input files. This
  allows `bb_worker` to cache these files across build actions,
  exposing it to the build action through hardlinking.
- To make execution pluggable. `bb_worker` communicates with `bb_runner`
  using [a simple gRPC-based protocol](https://github.com/buildbarn/bb-remote-execution/blob/main/pkg/proto/runner/runner.proto).
  One could, for example, implement a custom runner process that
  executes build actions using [QEMU user-mode emulation](https://www.qemu.org/).
- To work around [a race condition](https://github.com/golang/go/issues/22315)
  that effectively prevents multi-threaded processes from writing
  executables to disk and spawning them. Through this decomposition,
  `bb_worker` writes executables to disk, while `bb_runner` spawns them.

This repository provides container images for each of these components.
For `bb_runner`, it provides two images: `bb_runner_bare` and `bb_runner_installer`.
`bb_runner_bare` has no userland/linux install, it just has the `bb_runner`
executable. Typically the actions that will run on a runner do expect some
userland to be installed.

It would be nice if you could just use any image of your choosing as the image
that your build actions will run on. Like using
[Ubuntu 16.04 image](https://console.cloud.google.com/marketplace/details/google/rbe-ubuntu16-04),
to take advantage of the fact that bazel project provides [ready-to-use toolchain 
definitions](https://github.com/bazelbuild/bazel-toolchains) for them.

What makes that tricky is that that image will not have `bb_runner` installed.
This is where `bb_runner_installer` image comes in. It doesn't actually
install anything, but it provides the `bb_runner` executable through its
filesystem. You have to configure your orchestration of choice to mount this
filesystem from `bb_runner_installer` into the image of your choice that you
want to run on. This way you can use a vanilla image and just run the bb_runner
executable from Buildbarn's provided container. There's a few tricks to check
if the volume is already available, you can see an example of how to do this
in the [docker-compose example](https://github.com/buildbarn/bb-deployments/blob/e404c1a519355353d0e2cdfd447126fe07095594/docker-compose/docker-compose.yml#L89).

Please refer to [the Buildbarn Deployments repository](https://github.com/buildbarn/bb-deployments)
for examples on how to set up these tools.

---

## Fork additions: per-action cgroup enforcement and resource pooling

This fork extends upstream Buildbarn with **honest, kernel-enforced
resource limits for individual build actions**. The motivation: vanilla
Buildbarn matches actions to workers on platform-property *equality*,
but it has no notion of "this action will use 2 cores and 8 GiB" — once
the scheduler hands the action to a worker, the action shares the
runner container's full resource envelope with every other concurrent
action and can freely oversubscribe the node.

With this fork, when a client sets REv2 `exec_properties` like
`{ cpu: "2000", memory: "8Gi", gpu: "1" }`:

1. The **scheduler** still routes the action to a worker that advertises
   the matching `OSFamily`/`container-image`/etc., ignoring the
   resource hints when computing the queue key.
2. The **worker** reserves the requested cores/bytes/GPUs from an
   in-process pool before invoking the runner, blocking until they're
   free.
3. The **runner** creates a per-action cgroup v2 sub-cgroup
   (`/sys/fs/cgroup/bb-actions/run-<uuid>`), writes `cpu.max` /
   `memory.max` to the requested values, and atomically places the
   action's process tree into it via Linux's `CLONE_INTO_CGROUP`
   (`SysProcAttr.UseCgroupFD`) before `exec()`. The kernel then enforces
   throttling and OOM-kills against those limits for the duration of
   the action; the cgroup is torn down when the action exits.

### What's new, by component

#### 1. `RunRequest.resource_limits` — a new wire field on the worker→runner RPC
**File:** `pkg/proto/runner/runner.proto`

A `map<string, string>` carrying the per-action resource ask down to
the runner. Recognized keys: `cpu` (millicores, integer string),
`memory` (bytes, integer string), `gpu_uuids` (comma-separated UUIDs).
The map is empty for actions with no resource hints, in which case the
runner skips cgroup placement entirely (back-compat).

#### 2. `pkg/resourcepool` — per-worker admission control
**Files:** `pkg/resourcepool/pool.go`, `pkg/proto/configuration/bb_worker/bb_worker.proto`

A `sync.Cond`-backed semaphore that tracks free CPU millicores, memory
bytes, and a discrete set of GPU UUIDs. `Acquire(cpu, mem, gpuCount)`
blocks until the request fits and honors `ctx.Done()`; `Release` returns
the resources and wakes waiters. An ask that exceeds the configured
total fails immediately with a clear error rather than blocking forever.

Configured at worker startup via the new
`ApplicationConfiguration.resource_pool`:

```jsonnet
resourcePool: {
  cpuMillicores: 40000,                            // 40 cores
  memoryBytes: 450 * 1024 * 1024 * 1024,           // 450 GiB
  gpuUuids: ['GPU-31bb0c86-...', 'GPU-a22ff38a-...'],
},
```

If you don't set `resourcePool`, the worker behaves like upstream — no
admission control, no resource gating.

#### 3. `localBuildExecutor` — extracts the hints, gates on the pool
**Files:** `pkg/builder/local_build_executor.go`, `cmd/bb_worker/main.go`

`Execute()` parses `Action.Platform.Properties` (and
`Command.Platform.Properties` for newer REv2) looking for `cpu`,
`memory`, `gpu`. It supports K8s-style memory suffixes (`Ki`, `Mi`,
`Gi`, `K`, `M`, `G`). It then `pool.Acquire`s before invoking
`runner.Run` and `pool.Release`s in a `defer` — so the slot is held
for the entire active phase of the action and returned cleanly on
success, failure, or crash. The assigned GPU UUIDs are injected into
the action's environment as `NVIDIA_VISIBLE_DEVICES`.

#### 4. cgroup v2 placement in the runner
**Files:** `pkg/runner/local_runner.go`, `pkg/runner/local_runner_cgroup_linux.go`, `pkg/runner/local_runner_cgroup_other.go`, `pkg/proto/configuration/bb_runner/bb_runner.proto`, `cmd/bb_runner/cgroup_linux.go`, `cmd/bb_runner/cgroup_other.go`

On startup, if `cgroup_parent_path` is set in the runner config (e.g.
`/sys/fs/cgroup/bb-actions`), `bb_runner` walks the cgroup hierarchy
from the namespace root down to that path, enabling `+cpu +memory` in
each ancestor's `cgroup.subtree_control`. To satisfy cgroup v2's
no-internal-processes rule, it first evacuates its own PIDs to a
sibling `init` cgroup. This is a one-time setup at process start.

For every `RunRequest` with non-empty `resource_limits`, the runner:

1. Builds a `cgroup2.Resources` from the limits using
   `github.com/containerd/cgroups/v3`.
2. Creates `<cgroup_parent_path>/run-<uuid>` with `cpu.max` =
   `<millicores*100> 100000` and `memory.max` = `<bytes>`.
3. Opens an FD on the cgroup directory and sets
   `cmd.SysProcAttr.UseCgroupFD = true` + `CgroupFD = fd` — so the
   kernel places the child directly into the new cgroup with `clone3`
   instead of the racier post-fork move.
4. Runs the action, then `Kill()`s any survivors and `Delete()`s the
   cgroup on completion.

A non-Linux build of `bb_runner` uses a no-op stub for these calls and
refuses to start if `cgroup_parent_path` is configured.

#### 5. `FilteringActionKeyExtractor` — keeps resource hints out of the routing key
**Files:** `pkg/proto/configuration/scheduler/scheduler.proto`, `pkg/scheduler/platform/filtering_action_key_extractor.go`, `pkg/scheduler/platform/configuration.go`

Why this is needed: the scheduler's existing `action` key extractor
marshals the entire `Action.Platform` into the queue key. If an action
carries `{OSFamily, container-image, cpu=2000, memory=8Gi}`, it lands
in a brand-new queue that no worker advertises, and the client gets
`FAILED_PRECONDITION: No workers exist for ...`.

The new `filteringAction` extractor reads `Action.Platform` exactly
like the upstream extractor but **drops a configured list of property
names before computing the routing key**. The properties themselves
are still forwarded unchanged to the worker (where they drive the
pool acquire and the cgroup), so resource hints behave as hints
without sharding queues.

```jsonnet
actionRouter: {
  simple: {
    platformKeyExtractor: {
      filteringAction: {
        ignoredPropertyNames: ['cpu', 'memory', 'gpu', 'gpu_uuids'],
      },
    },
    ...
  },
},
```

#### 6. `image_load` Bazel targets — load OCI images straight into a local daemon
**Files:** `MODULE.bazel`, `cmd/bb_{worker,scheduler,runner,runner_installer}/BUILD.bazel`

Adds `rules_img 0.3.4` and an `image_load` target per binary so you
can `bazel run //cmd/bb_worker:bb_worker_container_load` and have the
image appear in your local containerd/docker daemon without pushing to
a registry. The corresponding tags are `bb-worker:cgroup-dev`,
`bb-scheduler:cgroup-dev`, `bb-runner-bare:cgroup-dev`,
`bb-runner-installer:cgroup-dev`. Useful for fast inner-loop iteration
against a local Kubernetes cluster.

### Operator setup checklist

To use the new features in a Kubernetes deployment:

1. **Schedule on a host with cgroup v2.** Most modern distros (RHEL 9,
   Ubuntu 22.04+, k3s/k8s 1.25+ defaults) are already there. Verify
   with `stat -fc %T /sys/fs/cgroup` → `cgroup2fs`.
2. **Grant the runner container `securityContext.privileged: true`.**
   Writing `cgroup.subtree_control` to enable the cpu/memory
   controllers requires `CAP_SYS_ADMIN` plus a writable cgroup mount.
   For local dev privileged is fine; for production, narrower
   capabilities + a kubelet-delegated cgroup are the right next step
   and not yet wired here.
3. **Set `cgroupParentPath` in the runner config** (jsonnet), e.g.
   `/sys/fs/cgroup/bb-actions`. The runner creates it on startup and
   evacuates its own PIDs to a sibling cgroup as needed.
4. **Set `resourcePool` in the worker config** with totals
   appropriate for the node (typically the pod's allocatable minus
   system overhead).
5. **Switch the scheduler's `platformKeyExtractor` to `filteringAction`**
   with `ignoredPropertyNames: ['cpu', 'memory', 'gpu', 'gpu_uuids']`.
6. **Tune `concurrency`.** The worker's concurrency value now caps the
   number of in-flight actions — including those *waiting* on the
   resource pool. Set it to roughly
   `ceil(cpu_pool_millicores / smallest_typical_request)` so big asks
   don't starve small ones out of available slots.

### Known limitations / out of scope

- **Pool fairness:** `Acquire` uses `cond.Broadcast`, so order between
  waiters is up to the Go runtime. A trickle of small asks can in
  principle starve a single large ask. Add a priority queue if you
  need strict fairness.
- **Scheduler visibility:** a slot blocked in `pool.Acquire` looks
  identical to a slot actively running in the scheduler UI — both
  report `Executing`. Distinguishing them would mean adding a new
  `WaitingForResources` state to `remoteworker.proto` and a small UI
  tweak; not done here.
- **GPU device gating** is currently env-var-only
  (`NVIDIA_VISIBLE_DEVICES`). True device cgroup gating on v2 needs an
  eBPF program; deferred.
- **cgroup v1** is not supported. The implementation is v2-only.
- **Stats reporting:** the cgroup's `memory.peak`, `cpu.stat`
  throttling counters, etc. are read by the test suite for
  verification but are not yet returned as REv2 `ExecutedActionMetadata`
  to the client.
