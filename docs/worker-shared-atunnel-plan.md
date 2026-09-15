# Worker-shared atunnel

The production gVisor and Cloud Hypervisor ateoms now embed one shared atunnel
implementation per worker. There is no additional process, sidecar, or node
service. The first commit supplies the socket primitives and kernel/runtime
fixtures; the second wires them into Run, Restore, Checkpoint, Terminate,
and shutdown.

## Configuration

Workers retain their existing single-actor runtime service, capacity reporting,
WorkerPool configuration, cgroups, and stats protocol. There is no multi-actor
RPC dispatcher. Supporting multiple actors per worker is a separate change;
the network primitives already support independently owned namespaces.

All sandbox TCP egress now requires a configured atenet gateway. Without one,
TCP and DNS fail closed; ingress-only sandboxes can still run. DNS is sent over
TCP through the actor's authenticated CONNECT tunnel to the worker resolver
from `/etc/resolv.conf`. Egress policy must allow that resolver on TCP port 53.
UDP DNS queries are translated to TCP; large UDP answers return TC so resolvers
retry over TCP. Arbitrary UDP and IPv6 egress are blocked.

The runtime namespace keeps the existing worker namespace path used by atelet
when preparing OCI bundles. No atelet or stats-protocol change is required.
Startup DNS requires the atenet authorization change: a RESUMING actor may use
egress only while its assignment belongs to an active, matching worker pod and
its actor UID remains in that worker's assignment list. Certificate and egress
policy checks still apply.

## Topology

```text
gVisor netstack -> runtime eth0 --veth-- gateway ateom0
MicroVM guest  -> virtio-net  --TAP--- gateway ateom0
                                      |
                           namespace-local capture sockets
                                      |
                            shared atunnel in ateom
                                      |
                          gateway-local TCP socket
                                      |
                        up0 --veth-- worker atw<N>
                                      |
                           worker SNAT -> atenet
```

Each sandbox retains guest address `169.254.17.2/30` and gateway
`169.254.17.1`. Sandbox netstacks need a default route to that gateway to deliver
intercepted traffic. **Gateway namespaces have no default route**, and their
IPv4/IPv6 FORWARD policy is DROP. Only atenet endpoint /32 routes are added;
application destinations never become routes. The trusted endpoint is resolved
on the worker, then its TCP socket is created inside the gateway namespace.

Worker-local transit /30s are allocated between `169.254.32.0` and
`169.254.159.255`, excluding existing routes and links. This avoids the sandbox
subnet and common cloud metadata addresses. Link-local, loopback, and unspecified
atenet endpoints are rejected.

PREROUTING REDIRECT captures sandbox TCP, with dedicated UDP/TCP DNS capture
before general TCP capture. SO_ORIGINAL_DST supplies the CONNECT destination.
Proxy sockets use OUTPUT rather than the capture chain, preventing recapture
without socket marks. gVisor gets its own runtime namespace because runsc consumes
its interfaces and runs an AF_PACKET netstack. MicroVMs use a TAP directly in the
gateway, with explicit fixed MAC and worker-uplink MTU; TC mirroring is removed.
Cloud Hypervisor starts in that gateway because it looks up the TAP by name.

Namespace switching is synchronous and short. TCP I/O uses Go's normal runtime
poller after namespace restoration; streams do not retain locked OS threads.

## Ownership and failures

The existing ateom service continues to handle Run, Restore, Checkpoint,
Terminate, stats, and graceful shutdown for one actor at a time.
`internal/ateomnet` owns namespace topology, routing, and capture rules.
`internal/ateomproxy` owns the worker ingress listeners, namespace-local TCP/DNS
listeners, readiness client, and actor certificate setup. Each live runtime
record holds one network session; that session publishes ingress after readiness
and resumes networking after a failed checkpoint. There is no separate actor
registry in atunnel.
The worker creates this network once; actor teardown removes the sandbox-facing
veth or TAP, and the next activation reuses the network and listeners.

Egress is active before application startup/readiness. Ingress is published only
after readiness succeeds. Preparation rolls back partial network setup, and the
runtime registers failure cleanup before preparing a network session. Failed
checkpoints reactivate networking only while the workload remains present,
obtaining a fresh certificate if it expired during the checkpoint. Namespace
cleanup follows runtime shutdown.

A proxy serving loop ending stops the other listeners and activations and reaches
the worker's fatal-error path; the worker cannot keep accepting work with a dead
proxy. Capture remains fail closed. In-process reconciliation after a process
crash is not implemented:
stale named namespaces are rejected rather than adopted under a new identity.
Replace a stale worker. Newly created workers use distinct namespace names.

## Validation

`TestProductionWorkerGVisor` and `TestProductionWorkerMicroVM` execute the actual
single-actor ateom RPC handlers. They cover failed startup/retry, ingress,
certificate minting and renewal, mTLS CONNECT egress, DNS-dependent readiness,
MTU, checkpoint/restore of application memory, invalid checkpoints, termination,
and sequential reuse of the same service and namespace by a different actor.
The VM test uses the real Kata image and agent. The test broker and CONNECT
endpoint use a local CA; these are not tests of a deployed atenet or Kubernetes
cluster. gVisor's isolated fixture disables host cgroup enforcement. Existing
namespace tests cover independent networks with same-address listeners,
destination recovery, forwarding bypass, cancellation, restoration, and
descriptor cleanup.

Provide runtime assets explicitly; tests never download them:

```sh
export GOFLAGS=-mod=readonly
export ATE_TEST_RUNSC=/path/to/gvisor/runsc
export ATE_TEST_MICROVM_ASSETS=/path/to/kata-assets
CGO_ENABLED=0 go build -o "$TMPDIR/network-probe" ./internal/ateomnet/testdata/network-probe
export ATE_TEST_NETWORK_PROBE="$TMPDIR/network-probe"
go test -race -c -o "$TMPDIR/gvisor.test" ./cmd/ateom-gvisor
go test -race -c -o "$TMPDIR/microvm.test" ./cmd/ateom-microvm
sudo -E "$TMPDIR/gvisor.test" -test.run TestProductionWorkerGVisor -test.count=10 -test.timeout=300s
sudo -E "$TMPDIR/microvm.test" -test.run TestProductionWorkerMicroVM -test.count=10 -test.timeout=300s
```

The Kata assets directory contains `cloud-hypervisor`, `vmlinux`, `rootfs.img`,
`configuration-clh.toml`, and `virtiofsd`. Tests require Linux root, TAP, and KVM
for MicroVM. Legacy pre-migration snapshots and thousands-of-actor throughput
have not been validated by these tests.
