# ateom-ch — Cloud Hypervisor microVM backend

`ateom-ch` is a gRPC server that runs actor workloads inside [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) microVMs. It implements the same core `ateom.Ateom` lifecycle interface as `ateom-gvisor`, plus a Cloud Hypervisor-specific template-preparation RPC for proactive golden snapshot creation.

## How it works

```
atelet
  │  gRPC (unix socket)
  ▼
ateom-ch ──► virtiofsd ──► OCI bundle rootfs (shared into VM as virtio-fs)
  │
  └──► cloud-hypervisor ──► Linux microVM (64 MiB RAM, 2 vCPUs)
              │
              └── guest kernel boots, mounts rootfs via virtiofs, exec's init
```

### Per-RPC lifecycle

**Startup (warmUp)**

Before gRPC starts serving, ateom-ch pre-warms a cloud-hypervisor process: CH starts and its API socket appears, but no VM is created yet. This means the first RunWorkload doesn't pay the ~50ms CH startup cost. After each actor teardown, a background goroutine immediately pre-warms a replacement CH.

**CreateTemplateGoldenSnapshot**

During Cloud Hypervisor `ActorTemplate` readiness, atelet prepares a template-scoped OCI bundle and calls `CreateTemplateGoldenSnapshot`. ateom-ch cold-boots with `goldeninit` as PID 1, waits for `.ateom-ready`, snapshots the idle VM into the template cache, and tears the VM down. Atelet then uploads that snapshot directory as one `.tar.zstd` object and records the URI in `ActorTemplate.status.goldenSnapshot`.

**RunWorkload — fast path (template golden snapshot supplied)**

When atelet starts a new actor from a prepared template, it downloads/caches the template golden snapshot locally and passes `template_golden_snapshot_path` to `RunWorkload` (~97ms):

1. Reads `config.json` to find the entrypoint.
2. Copies `golden-init` into the actor's rootfs; writes `.ateom-ready`; removes stale `.ateom-run-args`. This ensures the rootfs file tree matches the one virtiofsd had when the golden snapshot was taken (required for DEVICE_STATE compatibility).
3. Consumes the pre-warmed CH; starts virtiofsd + tap in parallel.
4. Calls `vm.restore` from `template_golden_snapshot_path`.
5. Calls `vm.resume` — goldeninit resumes as PID 1.
6. Writes `.ateom-run-args` (the workload entrypoint). goldeninit polls this file and `exec`s the workload within ~10ms.

If `template_golden_snapshot_path` is supplied but missing or invalid, `RunWorkload` fails instead of falling back to a cold boot.

**RunWorkload — legacy fallback slow path**

For direct ateom-ch tests or old callers that do not pass `template_golden_snapshot_path`, the first call for a given template still cold-boots with `goldeninit` as PID 1, captures a pod-local golden snapshot, and then restores/runs from it. This is a compatibility fallback; the production Cloud Hypervisor path should use `CreateTemplateGoldenSnapshot` during `ActorTemplate` readiness.

1. Reads `config.json` to find the entrypoint.
2. Copies `golden-init` into the actor's rootfs; removes `.ateom-ready` so we can detect when goldeninit writes it.
3. Consumes the pre-warmed CH; starts virtiofsd + tap in parallel.
4. Calls `vm.create` with `init=/golden-init`; calls `vm.boot`.
5. Polls the host-side rootfs for `.ateom-ready` (written by goldeninit through virtiofs when it becomes PID 1).
6. Calls `vm.pause` + `vm.snapshot` to the fallback golden directory.
7. Tears down the golden VM, restores from that snapshot, then writes `.ateom-run-args`; goldeninit exec's the workload.

**CheckpointWorkload**
1. Calls `vm.pause` — freezes all vCPUs.
2. Calls `vm.snapshot` — writes memory image + device state to `checkpoint-state/`.
   - virtiofsd saves its internal state via the vhost-user `DEVICE_STATE` protocol extension.
3. Tears down: shuts down CH, kills virtiofsd, deletes the tap device.
4. Starts a background doPrewarm goroutine for the next RestoreWorkload.

**RestoreWorkload**
1. Consumes the pre-warmed CH (if available; background doPrewarm may have completed by now).
2. Starts a fresh virtiofsd pointing at the same `bundle/rootfs/`, and creates the tap.
3. Calls `vm.restore` — CH reads the snapshot, sends saved virtiofsd state back via `DEVICE_STATE`, and rebuilds all virtio device state without guest involvement.
4. Calls `vm.resume` — vCPUs resume exactly where they paused.

### goldeninit

`goldeninit` (`hack/goldeninit/`) is the PID 1 binary used in golden VMs. It:

1. Writes `/.ateom-ready` — ateom-ch polls this on the host side (via virtiofs passthrough) and snapshots the idle VM once it appears.
2. Polls `/.ateom-run-args` in a 10ms loop. When ateom-ch writes the workload entrypoint there, goldeninit `exec`s it — replacing itself with the actual workload's PID 1.

goldeninit must be present in the actor rootfs at restore time with the same path it had at snapshot time (`/golden-init`), because virtiofsd's DEVICE_STATE records relative paths and tries to reopen them on restore.

### Why DEVICE_STATE matters

The vhost-user `DEVICE_STATE` protocol extension lets CH transfer virtiofsd's internal state (inode maps, open handles, vring positions) as an opaque blob during snapshot/restore. Without it, virtiofsd starts fresh with no knowledge of the prior session, and the PCI queue restore deadlocks waiting for guest-side acknowledgement that never arrives. This was fixed in:
- **cloud-hypervisor** ≥ v52.0 (PR #7908)
- **virtiofsd** ≥ v1.13 (built from `main`; Ubuntu 24.04's bundled v1.11 lacks it)

DEVICE_STATE blobs store **relative paths** (not inode numbers), so they are compatible across hosts or pods as long as the rootfs file tree is identical. For the golden snapshot path this is guaranteed because all actors of the same template share the same OCI image. For actor checkpoints (CheckpointWorkload/RestoreWorkload) it is guaranteed because atelet downloads the snapshot alongside the same OCI bundle.

### Networking

Each actor gets a **tap** device (`atch-ch0`) and a **bridge** (`atchbr-ch0`). A fixed name is used because the device name is embedded in CH snapshots — using the same name across RunWorkload and RestoreWorkload ensures the snapshot can be restored. The bridge joins the tap and the pod's `eth0`, giving the VM pod-level network connectivity. Both are torn down at checkpoint or on error.

### Paths

| Path | Purpose |
|------|---------|
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/bundles/<container>/` | OCI bundle (rootfs + config.json), written by atelet |
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/checkpoint-state/` | Snapshot output dir |
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/restore-state/` | Snapshot input dir (atelet downloads here) |
| `/var/lib/ateom-gvisor/templates/<ns>:<tmpl>/bundles/<container>/` | Template OCI bundle used for proactive CH golden creation |
| `/var/lib/ateom-gvisor/templates/<ns>:<tmpl>/cloud-hypervisor/golden/<container>/` | Local cache for downloaded/prepared CH template golden snapshot |
| `/var/lib/ateom-gvisor/ateoms/<pod-uid>/ateom.sock` | gRPC unix socket |
| `/run/ateom-ch/<pod-uid>/ch.sock` | Cloud Hypervisor API socket |
| `/run/ateom-ch/virtiofsd.sock` | virtiofsd vhost-user socket (fixed path — no pod-uid, because CH snapshots embed this path verbatim) |
| `/run/ateom-ch/<pod-uid>/golden-<ns>-<tmpl>-<container>/` | Legacy fallback pod-local golden snapshot directory |

### Expected latency

With the pre-warmed CH optimization and 64 MiB VM:

| Operation | Notes | Measured |
|-----------|-------|----------|
| CreateTemplateGoldenSnapshot | Template readiness: cold boot + golden snapshot | ~413ms |
| RunWorkload (fast path) | Golden restore: VmRestore + VmResume | ~97ms |
| RunWorkload (legacy slow path) | Compatibility fallback when no explicit template golden is supplied | ~413ms |
| RestoreWorkload | Actor checkpoint restore: VmRestore + VmResume | ~123ms |
| CheckpointWorkload | VmPause + VmSnapshot (64 MiB memory) | ~600ms |

virtiofsd and tap creation run in parallel with CH startup (or with VmRestore after CH is pre-warmed), hiding most of their latency. With proactive template preparation, the cold-boot cost is paid during `ActorTemplate` readiness rather than on first actor creation.

---

## Running the end-to-end test

The integration test spins up a disposable kind cluster, builds the Docker image, and exercises the full `CreateTemplateGoldenSnapshot → RunWorkload(golden) → Checkpoint → Restore → Checkpoint → RunWorkload(golden)` lifecycle inside a privileged pod.

### Prerequisites

| Tool | Install |
|------|---------|
| Docker | https://docs.docker.com/get-docker/ |
| kind | `go install sigs.k8s.io/kind@latest` or https://kind.sigs.k8s.io |
| kubectl | https://kubernetes.io/docs/tasks/tools/ |
| `/dev/kvm` | Must be available on the host (hardware virt or nested virt enabled) |

Check KVM is accessible:

```bash
ls -l /dev/kvm          # should exist
kvm-ok                  # optional, from cpu-checker package
```

### Run the test

```bash
./hack/ateom-ch-kind-test.sh
```

This takes 2–4 minutes on first run (downloads ~200 MB: CH binary, kernel, virtiofsd, grpcurl). Subsequent runs are faster due to Docker layer caching.

Pass `--keep` to leave the cluster running after the test (useful for manual inspection):

```bash
./hack/ateom-ch-kind-test.sh --keep
```

### What the test does step by step

```
1. Create kind cluster 'ateom-ch-test'
     extraMounts: /dev/kvm, /dev/net/tun  ← required for KVM and tap networking

2. docker build build/ateom-ch/Dockerfile
     → compiles ateom-ch + testinit + goldeninit
     → downloads cloud-hypervisor v52.0, virtiofsd (latest), kernel, grpcurl

3. kind load docker-image ateom-ch:test

4. kubectl apply privileged pod (runAsUser: 0, securityContext.privileged: true)
     volumeMounts:
       /var/lib/ateom-gvisor  ← OCI bundles, checkpoint state
       /run/ateom-ch          ← CH + virtiofsd sockets

5. Inside the pod:
   a. Write a template OCI bundle and two actor OCI bundles (actor1 + actor2, same template):
      rootfs/ contains only 'testinit' binary as /init
      config.json sets process.args = ["/init"]

   b. Start ateom-ch --pod-uid test-pod-<timestamp>
      warmUp: pre-warms a CH process before gRPC starts serving
      Wait for gRPC unix socket to appear

   c. grpcurl CreateTemplateGoldenSnapshot
      → uses the template bundle rootfs
      → copies golden-init into rootfs; removes .ateom-ready
      → uses pre-warmed CH; virtiofsd + tap start concurrently
      → VM boots with init=/golden-init; goldeninit writes .ateom-ready
      → vm.pause + vm.snapshot → golden snapshot saved
      → VM is torn down

   d. grpcurl RunWorkload(actor1) — explicit template golden restore
      → passes template_golden_snapshot_path
      → vm.restore from template golden + vm.resume
      → .ateom-run-args written; goldeninit exec's testinit

   e. sleep 5s

   f. grpcurl CheckpointWorkload(actor1)
      → vm.pause → vm.snapshot → teardown
      → snapshot files appear in checkpoint-state/
      → background doPrewarm starts CH for next operation

   g. cp checkpoint-state/ → restore-state/
      (simulates atelet downloading the snapshot)

   h. grpcurl RestoreWorkload(actor1)
      → pre-warmed CH consumed; virtiofsd + tap start in parallel
      → vm.restore loads snapshot + transfers virtiofsd DEVICE_STATE
      → vm.resume — VM continues from paused point

   i. sleep 3s

   j. grpcurl CheckpointWorkload(actor1) — second checkpoint
      → proves the restored VM is fully healthy

   k. grpcurl RunWorkload(actor2) — explicit template golden restore
      → copies golden-init; writes .ateom-ready; removes .ateom-run-args
      → vm.restore from template golden + vm.resume
      → .ateom-run-args written; goldeninit exec's testinit

   l. sleep 2s

   m. grpcurl CheckpointWorkload(actor2)
      → proves golden-restore VM is healthy

   n. grep ateom-ch logs to verify no lazy "RunWorkload: cold boot" occurred

6. Print PASS / FAIL
```

### Expected output (end)

```
── CreateTemplateGoldenSnapshot ──────────────────────────────────────
  CreateTemplateGoldenSnapshot: ~413ms

── RunWorkload actor1 (template golden restore) ──────────────────────────────────────
  RunWorkload (template golden restore): ~97ms

── CheckpointWorkload ──────────────────────────────────────
  CheckpointWorkload: ~600ms

── RestoreWorkload ──────────────────────────────────────
  RestoreWorkload: ~123ms
  Second checkpoint succeeded — restored VM is healthy.

── RunWorkload actor2 (fast path — golden restore) ──────────────────────────────────────
  RunWorkload (fast/golden restore): ~97ms
  actor2 checkpoint succeeded — golden-restore VM is healthy.
  Verified: no lazy RunWorkload cold boot occurred.

────────────────────────────────────────────────
PASS: ateom-ch CreateTemplateGoldenSnapshot → RunWorkload(golden) → Checkpoint → Restore → Checkpoint → RunWorkload(golden)
────────────────────────────────────────────────
```

### Inspecting a failed run

Keep the cluster and exec into the pod:

```bash
./hack/ateom-ch-kind-test.sh --keep

kubectl exec -it ateom-ch-test -- bash

# Inside the pod:
ls /var/lib/ateom-gvisor/actors/   # OCI bundles + snapshot state
ls /run/ateom-ch/                  # CH + virtiofsd sockets
virtiofsd --version                # should be v1.13+
cloud-hypervisor --version         # should be v52.0

# Manual gRPC call:
grpcurl -plaintext -unix \
  /var/lib/ateom-gvisor/ateoms/<pod-uid>/ateom.sock \
  list ateom.Ateom
```

Delete the cluster when done:

```bash
kind delete cluster --name ateom-ch-test
```

---

## Image contents

| Binary | Version | Source |
|--------|---------|--------|
| `ateom-ch` | from source | `cmd/ateom-ch/` |
| `golden-init` | from source | `hack/goldeninit/` — PID 1 for golden VMs |
| `cloud-hypervisor` | v52.0 | GitHub releases (static) |
| `virtiofsd` | latest `main` | GitLab CI artifact (static musl) |
| guest kernel | ch-release-v6.16.9-20260508 | cloud-hypervisor/linux releases |
| `grpcurl` | 1.9.1 | GitHub releases |
| `testinit` | from source | `hack/testinit/` (test use only) |

## Key version constraints

- **cloud-hypervisor ≥ v52.0** — required for `DEVICE_STATE` restore support (PR #7908).
- **virtiofsd ≥ v1.13** — required for `DEVICE_STATE` protocol support on the backend side. The Ubuntu 24.04 package (v1.11.1) does not work correctly with CH v52 restore; use the upstream static binary.
- **Kernel with `CONFIG_VIRTIO_FS=y`** — the CH team's CI kernel has this; stock distro kernels may not.
