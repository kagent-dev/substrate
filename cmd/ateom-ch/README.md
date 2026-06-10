# ateom-ch — Cloud Hypervisor microVM backend

`ateom-ch` is a gRPC server that runs actor workloads inside [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) microVMs. It implements the same `ateom.Ateom` gRPC interface as `ateom-gvisor`, so atelet can drive it without changes.

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

**RunWorkload**
1. Reads `config.json` from the OCI bundle to find the entrypoint.
2. Consumes the pre-warmed CH (if available) or starts CH fresh.
3. Starts **virtiofsd** pointing at `bundle/rootfs/` and creates the tap — both happen while CH is (possibly) still starting.
4. Calls `PUT /api/v1/vm.create` with:
   - The CH kernel (`vmlinux`), cmdline `root=rootfs rootfstype=virtiofs rw init=<entrypoint>`
   - A `fs` device wired to the virtiofsd socket (tag `rootfs`)
   - `memory.shared = true` (memfd-backed shared memory so virtiofsd can access guest RAM)
   - A tap network device bridged to `eth0`
5. Calls `PUT /api/v1/vm.boot` — guest kernel starts and mounts virtiofs as `/`.

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

### Why DEVICE_STATE matters

The vhost-user `DEVICE_STATE` protocol extension lets CH transfer virtiofsd's internal state (inode maps, open handles, vring positions) as an opaque blob during snapshot/restore. Without it, virtiofsd starts fresh with no knowledge of the prior session, and the PCI queue restore deadlocks waiting for guest-side acknowledgement that never arrives. This was fixed in:
- **cloud-hypervisor** ≥ v52.0 (PR #7908)
- **virtiofsd** ≥ v1.13 (built from `main`; Ubuntu 24.04's bundled v1.11 lacks it)

DEVICE_STATE blobs are filesystem-specific (they contain inode mappings). This means a snapshot taken with virtiofsd serving directory A can only be restored with a virtiofsd also serving directory A. ateom-ch satisfies this by using the same OCI bundle rootfs path in both CheckpointWorkload and RestoreWorkload.

### Networking

Each actor gets a **tap** device (`atch-ch0`) and a **bridge** (`atchbr-ch0`). A fixed name is used because the device name is embedded in CH snapshots — using the same name across RunWorkload and RestoreWorkload ensures the snapshot can be restored. The bridge joins the tap and the pod's `eth0`, giving the VM pod-level network connectivity. Both are torn down at checkpoint or on error.

### Paths

| Path | Purpose |
|------|---------|
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/bundles/<container>/` | OCI bundle (rootfs + config.json), written by atelet |
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/checkpoint-state/` | Snapshot output dir |
| `/var/lib/ateom-gvisor/actors/<ns>:<tmpl>:<id>/restore-state/` | Snapshot input dir (atelet downloads here) |
| `/var/lib/ateom-gvisor/ateoms/<pod-uid>/ateom.sock` | gRPC unix socket |
| `/run/ateom-ch/<pod-uid>/ch.sock` | Cloud Hypervisor API socket |
| `/run/ateom-ch/<pod-uid>/virtiofsd.sock` | virtiofsd vhost-user socket |

### Expected latency

With the pre-warmed CH optimization and 64 MiB VM:

| Operation | Breakdown | Target |
|-----------|-----------|--------|
| RunWorkload | vf start 10ms ‖ tap 5ms, then VmCreate 1ms + VmBoot ~80ms | ~90ms |
| RestoreWorkload | vf start 10ms ‖ tap 5ms, then VmRestore ~50ms + VmResume 10ms | ~70ms |
| CheckpointWorkload | VmPause + VmSnapshot (128 MiB memory) | ~600ms |

virtiofsd and tap creation run in parallel with CH startup (or with VmRestore after CH is pre-warmed), hiding most of their latency.

---

## Running the end-to-end test

The integration test spins up a disposable kind cluster, builds the Docker image, and exercises the full `RunWorkload → Checkpoint → Restore → Checkpoint` lifecycle inside a privileged pod.

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
     → compiles ateom-ch + testinit
     → downloads cloud-hypervisor v52.0, virtiofsd (latest), kernel, grpcurl

3. kind load docker-image ateom-ch:test

4. kubectl apply privileged pod (runAsUser: 0, securityContext.privileged: true)
     volumeMounts:
       /var/lib/ateom-gvisor  ← OCI bundles, checkpoint state
       /run/ateom-ch          ← CH + virtiofsd sockets

5. Inside the pod:
   a. Write OCI bundle: rootfs/ contains only 'testinit' binary as /init
      config.json sets process.args = ["/init"]

   b. Start ateom-ch --pod-uid test-pod-<timestamp>
      warmUp: pre-warms a CH process before gRPC starts serving
      Wait for gRPC unix socket to appear

   c. grpcurl RunWorkload
      → uses pre-warmed CH (no startup delay)
      → virtiofsd starts, sharing rootfs/; tap created concurrently
      → VM boots with virtiofs root; testinit runs as PID 1

   d. sleep 5s  (let VM settle, testinit runs)

   e. grpcurl CheckpointWorkload
      → vm.pause → vm.snapshot → teardown
      → snapshot files appear in checkpoint-state/
      → background doPrewarm starts CH for next RestoreWorkload

   f. cp checkpoint-state/ → restore-state/
      (simulates atelet downloading the snapshot)

   g. grpcurl RestoreWorkload
      → pre-warmed CH consumed; virtiofsd + tap start in parallel
      → vm.restore loads snapshot + transfers virtiofsd DEVICE_STATE
      → vm.resume  — VM continues from paused point

   h. sleep 3s

   i. grpcurl CheckpointWorkload  (second checkpoint of restored VM)
      → proves the restored VM is fully healthy

6. Print PASS / FAIL
```

### Expected output (end)

```
── RestoreWorkload ──────────────────────────────────────
  RestoreWorkload: ~70ms

  Waiting 3s to verify restored VM is alive...
  Second checkpoint succeeded — restored VM is healthy.

────────────────────────────────────────────────
PASS: ateom-ch RunWorkload → Checkpoint → Restore → Checkpoint
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
| `cloud-hypervisor` | v52.0 | GitHub releases (static) |
| `virtiofsd` | latest `main` | GitLab CI artifact (static musl) |
| guest kernel | ch-release-v6.16.9-20260508 | cloud-hypervisor/linux releases |
| `grpcurl` | 1.9.1 | GitHub releases |
| `testinit` | from source | `hack/testinit/` (test use only) |

## Key version constraints

- **cloud-hypervisor ≥ v52.0** — required for `DEVICE_STATE` restore support (PR #7908).
- **virtiofsd ≥ v1.13** — required for `DEVICE_STATE` protocol support on the backend side. The Ubuntu 24.04 package (v1.11.1) does not work correctly with CH v52 restore; use the upstream static binary.
- **Kernel with `CONFIG_VIRTIO_FS=y`** — the CH team's CI kernel has this; stock distro kernels may not.
