# Worker-shared atunnel implementation plan

Status: proposed, based on upstream checkout `26c38616`. Scope: one shared atunnel
implementation embedded in each worker's ateom process, serving every sandbox
in that worker. The kernel-boundary proof below is implemented; runtime
integration and multi-sandbox capacity remain planned.

## Implementation progress

The first change adds IPv4 namespace-aware `ListenTCP` and `DialTCP` helpers in
`internal/ateomnet/socket_linux.go`, reusing the existing mdlayher/socket
dependency. They return standard Go TCP listeners/connections; namespace
switching ends before bind/connect and application I/O.

Root-gated tests now exercise two sandbox gateways in one process, using both
veth and direct TAP attachments. They verify identical listener addresses,
original destination/port recovery through atunnel, gateway-specific upstream
source addresses, ingress delivery, half-close forwarding, and a surviving
connection while the other sandbox's network is removed. Additional socket
tests cover input rejection, connect cancellation, namespace-handle lifetime,
caller namespace preservation, and descriptor cleanup on success and failure.

`TestSandboxGatewayTLS` also exercises production atunnel interception and
mTLS CONNECT with simultaneous streams from both namespaces, distinct client
certificates, payloads larger than relay buffers, half-close forwarding, and
proxy restart. Capture stays installed while the proxy is stopped; new actor
connections fail instead of bypassing it. Its test gateway verifies the client
identity against the source namespace and verifies the CONNECT authority.

The TLS/runtime fixtures have no default route in their gateway namespaces.
Only the TLS egress endpoint has an explicit remote host route; the original
application destination is deliberately unroutable there. The new
`InstallGatewayNftablesRules` helper captures IPv4 TCP on the sandbox-facing
interface and drops all IPv4/IPv6 forwarding without compatibility masquerade.
Root checks confirm direct connections to the unrouted destination fail and
actor UDP cannot escape to a connected transit peer or the allowed endpoint.
Sandbox netstacks retain a default route toward their local gateway.

Opt-in runtime tests boot two real gVisor sandboxes or two Cloud Hypervisor VMs.
Both run concurrent ingress/egress through the namespace-aware mTLS client.
They checkpoint and restore A's in-memory counter while B serves traffic, then
remove A and check B again. The VM restore supplies a newly created TAP and
preserves the configured 1400-byte MTU. Validated runtimes: gVisor build
`release-20260824.0-120-g727c8c389c36-dirty`, Cloud Hypervisor v53.0, and the Kata
4.0.0 asset set's Linux 6.18.35 kernel on Linux/amd64 with KVM.

The VM uses a minimal Linux guest probe, not kata-agent. The TLS gateway uses
a test CA and echo endpoint, not the credential broker or deployed atenet.
These checks validate the runtime network boundary; ateom lifecycle wiring,
worker admission, migration of existing production snapshots, and multiple
placements remain unimplemented. Deployed behavior and capacity are unchanged.

To reproduce, set `ATE_TEST_RUNSC` to the runsc binary from a complete gVisor
bundle, `ATE_TEST_CLOUD_HYPERVISOR` to the VMM binary, and `ATE_TEST_VM_KERNEL`
to the guest kernel. Runtime tests skip when these assets are not configured.
The host needs root, KVM, TAP, `nsenter`, and `/usr/sbin/mkfs.ext4`.

```sh
export GOFLAGS=-mod=readonly
CGO_ENABLED=0 go build -o "$TMPDIR/network-probe" ./internal/ateomnet/testdata/network-probe
go test -race -c -o "$TMPDIR/ateomnet.test" ./internal/ateomnet
sudo -E env "TMPDIR=$TMPDIR" "ATE_TEST_NETWORK_PROBE=$TMPDIR/network-probe" \
  "$TMPDIR/ateomnet.test" -test.v -test.shuffle=on -test.count=10 -test.timeout=300s
```

`-mod=readonly` uses modules instead of this checkout's incomplete vendor tree
and is compatible with license verification's temporary Go workspace. Run
`env -u NO_COLOR GOFLAGS=-mod=readonly make verify` from a clean committed
checkout; the existing color-output tests require `NO_COLOR` to be unset.

## Recommendation

Keep `internal/atunnel` embedded in ateom, as it is today. Ateom remains responsible
for sandbox lifecycle and network configuration. Give each sandbox a Linux
gateway namespace, and create its egress listener and upstream sockets there.
The one ateom process serves all these sockets from its ordinary Go runtime.
Namespace-associated sockets require no separate proxy process or container.

Keep the existing sandbox-facing address, ingress protocols, egress gateway,
and actor certificates. This adopts namespace-associated sockets without
introducing Istio, HBONE, TPROXY, or a new node service.

## What the current code actually does

| Area | Current behavior | Relevant code |
| --- | --- | --- |
| Proxy placement | atunnel is a library hosted by each ateom process | `cmd/ateom-gvisor/main.go:runAtunnel`, `cmd/ateom-microvm/main.go:do` |
| Network | One worker-side `ateom0`, one interior namespace, fixed actor IP `169.254.17.2/30`, gateway `169.254.17.1` | `internal/ateomnet/net.go:SetupActorNetwork` |
| Capture | IPv4 nftables PREROUTING REDIRECT to port 15001 in the worker namespace; non-tunneled traffic is masqueraded, with UDP restricted to destination port 53 | `internal/ateomnet/net.go:InstallActorNftablesRules` |
| Ingress | Worker listeners on 443 and 8443 authenticate the router and forward to the active actor | `internal/atunnel/ingress.go` |
| Egress | SO_ORIGINAL_DST supplies the destination; a worker-authenticated broker mints an actor certificate; TLS CONNECT goes to the egress gateway | `internal/atunnel/{original_dst_linux,credential,client,egress}.go` |
| Runtime | gVisor uses its own network stack; microVMs use Cloud Hypervisor with TAP/TC mirroring in the interior namespace | `cmd/ateom-gvisor/runsc.go`, `cmd/ateom-microvm/net.go` |
| Multiplexing | Proxy activation and several ateom fields hold one actor; reported actor capacity is explicitly 1 | `internal/atunnel/{ingress,egress}.go`, both ateom service structs, `internal/ateomcapacity/ateomcapacity.go` |

The supplied Firecracker topology is not the topology of this checkout.
Moving listeners into the existing interior namespace is also insufficient:
the VM guest has a separate kernel, and the current TAP/TC cross-connect does
not route guest traffic through that namespace's normal Linux TCP stack.
Host namespace loopback is not application loopback for either sandbox runtime.

### gVisor attachment: AF_PACKET first, TAP optional

The [gVisor networking guide](https://gvisor.dev/docs/architecture_guide/networking/)
describes runsc copying interface configuration into netstack and using
AF_PACKET for non-loopback traffic. Upstream
[`runsc/sandbox/network.go`](https://github.com/google/gvisor/blob/master/runsc/sandbox/network.go)
also removes the copied addresses from host interfaces. Its interface discovery
skips down interfaces and non-loopback interfaces without usable addresses;
"every interface" is a useful warning rather than the exact selection rule.

Keep the runtime namespace limited to that sandbox's attachment and loopback.
Connect it by veth to the gateway namespace, where Linux retains the gateway
addresses, routing, capture rules and proxy sockets. Runsc must never scrape
the gateway namespace or the shared worker namespace. Actor egress crosses
the veth and enters Linux PREROUTING in the gateway namespace; it does not
traverse host OUTPUT as an ordinary application TCP socket would.

Netstack's
[`fdbased` endpoint](https://github.com/google/gvisor/blob/master/pkg/tcpip/link/fdbased/endpoint.go)
supports TAP/TUN descriptors. That library capability does not establish a
drop-in TAP option in Substrate's runsc integration: the inspected upstream
runsc setup uses AF_PACKET, and Substrate does not select a TAP mode. These
upstream source observations are separate from validation of Substrate's
pinned September 2, 2026 runsc release.

Use the existing AF_PACKET attachment for the first implementation. A direct
TAP attachment could later simplify the topology, potentially removing the
runtime attachment namespace/veth, but requires verifying runsc configuration
or FD injection, packet/offload settings, and checkpoint/restore support. TAP
does not itself move application TCP sockets or loopback into the host kernel.

### MicroVM attachment: TAP directly in the gateway namespace

Use one host gateway namespace per VM. The guest kernel already provides the
separate application network stack, so this path does not need gVisor's runtime
attachment namespace or its sandbox-facing veth pair.

Create the TAP in the gateway namespace, assign it `169.254.17.1/30`, and give
its open queue FDs to Cloud Hypervisor using the existing network-FD APIs. The
guest retains `169.254.17.2/30` on its virtio-net device. Packets written by the
VMM to the TAP enter the gateway's Linux stack and PREROUTING capture directly.
Linux sends application-bound packets out the TAP for the VMM to deliver to
the guest. Remove the current TAP-to-veth TC redirects; they would bypass this
local Linux processing.

Launch Cloud Hypervisor in the gateway namespace too. Its TAP-FD data path is
namespace-associated, but it also queries TAP properties by interface name
using a control socket in its process namespace. Running it outside the
gateway caused an MTU lookup failure in the runtime test. Entering the gateway
for VMM startup preserves MTU advertisement; atunnel stays in the shared ateom
process. Apply the same process namespace placement on restore.

Preserve `actorGuestMAC` on the guest device and assign the existing gateway
MAC (`hostVethMAC`) to the TAP. Both IPs and both MACs must remain stable across
restores because guest snapshots retain interface configuration and neighbor
state. Recreate the TAP and supply fresh queue FDs before boot or restore;
preserve compatible MTU, queue count, virtio header and offload settings. Test
restoring existing snapshots into this topology before claiming compatibility.

## Proposed topology

```text
Worker pod network namespace
  pod eth0 -> Kubernetes network
  ateom process
    sandbox lifecycle + namespace / veth / nftables ownership
    embedded atunnel
      shared mTLS ingress :443 / :8443
      actor identity -> sandbox session
  transit veth A                       transit veth B
       |                                    |
  gateway namespace A                  gateway namespace B
    unique transit IP                    unique transit IP
    ateom0: 169.254.17.1                  ateom0: 169.254.17.1
    capture socket :15001                 capture socket :15001
    namespace-local upstream sockets     namespace-local upstream sockets
       | veth                               | TAP FD / virtio-net
  runtime namespace A                  VM B (separate guest kernel)
    gVisor AF_PACKET endpoint             guest eth0: 169.254.17.2
    netstack: 169.254.17.2
```

The sockets shown inside the gateway namespaces belong to the one ateom
process. No proxy process runs inside either gateway namespace.

Each gateway namespace recreates the network role currently played by the
worker pod namespace and allows overlapping sandbox addresses. gVisor retains
its separate runtime namespace and AF_PACKET attachment; microVMs attach their
TAP directly to the gateway's Linux stack. The diagram shows one of each to
compare the paths; it does not require mixed runtime support in a worker.

Allocate a unique transit /30 per live sandbox from a worker-local prefix that
does not overlap reachable cluster, service, or required external ranges.
Reuse the pool across workers. Worker-side interface names must be unique;
fixed names can remain inside each gateway and runtime namespace. Record
allocation ownership in the sandbox session and release it on teardown.

Actor TCP enters its gateway namespace through `ateom0` for gVisor or the TAP
for microVMs, hits PREROUTING
REDIRECT, and reaches that namespace's atunnel socket. Atunnel recovers the
original destination and opens its connection to the existing egress gateway
from the same namespace. For ingress, the shared worker listener chooses a
sandbox by actor identity and dials `169.254.17.2:<port>` in its gateway namespace.

Retain PREROUTING capture. Locally created proxy connections traverse OUTPUT,
so they should not re-enter this capture rule. Match the sandbox-facing input
interface as well as the actor source address. Prove remote egress and local
application delivery do not loop. SO_MARK and an OUTPUT exemption are needed
only if capture later expands to OUTPUT; introduce the mark and its rules
together in that change.

Do not install a default route in a gateway namespace. Add only explicit
remote host routes to the configured atenet egress endpoints via the transit
peer; retain the connected routes needed for proxy-to-application delivery.
Resolve and reconcile endpoint addresses through the worker's trusted network.
Namespace-local proxy connections still use those restricted routes. Worker
transit-to-pod SNAT may be required by the CNI for these proxy connections.

The gVisor netstack or VM guest must retain its default route to
`169.254.17.1`: it sends arbitrary application destinations toward capture.
Removing that route would fail in the sandbox before PREROUTING interception.
This is distinct from a default route out of the gateway namespace.

Route restriction alone does not stop bypass to connected destinations. The
gateway's FORWARD policy must drop both IPv4 and IPv6; atunnel uses local
INPUT/OUTPUT and needs no packet forwarding. Do not carry over the UDP DNS
masquerade or direct-egress fallback. Missing egress configuration must prevent
enrollment. DNS requires an explicit local proxy path before rollout; keeping
the existing direct UDP resolver path would violate the all-egress-through-proxy
target. The current runtime tests use numeric destinations and do not implement
that DNS path.

## Ownership and interfaces

**Ateom owns each sandbox's network and lifecycle.** Replace the shared
`interiorNetNS` with a session-owned gateway handle, a runtime namespace handle
for gVisor, transit allocation, and cleanup state. Scope setup and cleanup to that session:
current cleanup deletes a fixed interface and a whole per-worker nftables
table, which would disrupt other sandboxes.

Use a sandbox identity containing worker UID, actor UID, and activation
generation. An actor UID alone cannot distinguish a delayed teardown from a
later activation of the same actor. Namespace device/inode identity accompanies
the held FD; PID and FD integer values are not durable identifiers.

**Atunnel owns proxy state.** Keep a registry of sandbox sessions. Each contains
its borrowed namespace handle, egress listener, actor certificate and renewal, cancellation
state, active connections, and HTTP upstream transports. Preserve separate
connection pools: every sandbox has the same application IP, so a global pool
keyed only by destination would mix sandboxes.

Keep a shared worker ingress frontend. Authenticate the router first, resolve
its `atenet.TargetActorHeader` to the active sandbox, then use that session's
handler and dialer. Preserve target-port validation, the client's Host,
HTTP/1, h2c/gRPC, CONNECT behavior, and stale-assignment responses. Bind handler
work to its generation so actor replacement cannot reuse old upstream state.

**Use direct Go calls between ateom and atunnel.** Ateom opens and owns the
gateway namespace handle and supplies it with trusted actor identity, activation
generation, and gateway configuration. Atunnel borrows the handle for the
session's lifetime. Deactivation must stop and join all users of the handle
before ateom closes it; a timed-out teardown cannot release a handle still in
use. Make preparation, ingress activation, and teardown generation-specific
and idempotent. Return readiness and cleanup errors directly to the lifecycle
caller. No Unix control socket, SCM_RIGHTS protocol, or separate enrollment
reconciliation is needed within this process.

**Keep the worker's credentials.** Retain the existing worker PodCertificate
projection. Atunnel continues to generate actor private
keys itself and authenticate to atelet's credential broker as that worker.
The broker sends the actor reference and UID to the authoritative assignment
check, which queries worker/actor assignments. Preserve those
checks and test two active actors renewing independently.

## Socket implementation

Add a small Linux namespace socket helper and inject its dial function into
the proxy. Egress already has `atunnel.WithDialer`; ingress needs the equivalent
for both HTTP transports and the explicit CONNECT dial in `ServeConnectHTTP`.
Readiness probes also need a sandbox-specific dialer once the worker no longer
has a unique route to `169.254.17.2`.

The vendored `github.com/mdlayher/socket` already supports `Config.NetNS`,
context-aware connect, and safe disposal of a thread whose namespace cannot be
restored. Start there rather than writing a new asynchronous socket engine.
Adapt descriptors to `net.Listener`/`net.Conn` using standard FD wrappers and
close intermediate duplicates. Keep accepted connections compatible with the
current original-destination resolver, which expects `*net.TCPConn`.

Socket creation must happen in the selected namespace. `net.Dialer.Control`
is too late to change socket ownership. Do not assume wrapping an arbitrary
Go dial in `NetNSDo` covers resolver or parallel dial goroutines. Create the
socket in the namespace, restore the thread, then connect and perform I/O.
Resolve configured gateway names deliberately: setns does not change
`resolv.conf`, and namespace-aware TCP does not automatically cover DNS.

Use ateom's existing namespace and network privileges. Validate namespace
socket creation against the existing seccomp and capability profile; a new
container security context is unnecessary.

## Lifecycle and failure behavior

1. Reserve a generation and transit allocation; create its namespaces and links
   with actor traffic gated. Prepare credentials and the embedded proxy session.
2. Bind the egress listener and return proxy readiness. Install capture
   and enable configured egress before starting or resuming application code.
3. Start/restore the sandbox and run readiness checks through its gateway
   namespace. Enable ingress after application readiness, then complete the RPC.
4. On checkpoint or termination, reject new admissions for that generation,
   close/drain its streams and transports, and complete deactivation before
   destroying its network. Do not affect sibling sessions.
5. On setup failure, undo only that generation's allocations, listeners,
   credentials, and namespace references. A delayed cleanup must not remove a
   replacement session. Do not reuse a namespace with stale listener backlog
   or conntrack state from a previous activation.

Today both ateoms enable egress together with ingress after application
readiness. Split those gates so applications that need egress during startup
can become ready.

On a session's listener or proxy failure, stop its admissions and surface the
failure to ateom without deleting capture rules or disturbing sibling sessions.
Captured TCP must fail closed; other traffic retains its configured policy.
An unrecovered process crash loses ateom and all its embedded proxy sessions
together. The first version may tear down/recycle that worker rather than adopt
running sandboxes, but must retain capture until those sandboxes are stopped
and clean up surviving runtime processes and namespace references. Full runtime
adoption is separate. There is no independent proxy restart to coordinate.

## Implementation sequence

| Step | Deliverable | Completion check |
| --- | --- | --- |
| 1. Prove the topology | Root-gated Linux tests for veth-connected runtime namespaces and direct TAP attachment, with two sandbox gateways, identical actor IPs and capture ports, and one proxy process | Real TCP reaches the correct application and remote destination; SO_ORIGINAL_DST and no-loop checks pass |
| 2. Scope network state | Session-owned namespace paths, veth allocation, NAT, cleanup and namespace dial helper | Creating/removing A leaves B's links, flows and routes intact |
| 3. Make ateom sandbox-aware | Per-actor runtime sessions, attribution, readiness, failure cleanup and shutdown | Both runtimes can retain two live sandboxes without one lifecycle operation replacing the other |
| 4. Share embedded atunnel | Per-sandbox proxy registry, direct lifecycle calls, namespace dialers, shared worker ingress | Two actors share the worker process and retain separate connections, identities and certificate renewal |
| 5. Handle failure and restart | Session-specific failure handling, worker readiness, process restart cleanup | One session's failure leaves the other operational; process restart fails closed and cleans up the worker |
| 6. Enable multiple placements | Explicit worker sandbox limit, resource accounting and capacity reporting | Two actor placements run on one worker; independent suspend/restore and migration pass for gVisor and microVM |

Keep reported capacity at one until all preceding isolation and lifecycle gates
pass. A global ateom lifecycle lock can initially remain: serializing boot and
checkpoint RPCs does not prevent multiple already-running sandboxes from
serving traffic. Introduce per-sandbox lifecycle concurrency only when needed;
session ownership and targeted cleanup are required immediately.

The main implementation locations are:

- `internal/ateomnet/net.go`: split reusable sandbox-facing setup from transit
  setup; scope nftables and cleanup to the correct namespace.
- `internal/ateompath/ateompath.go` and
  `cmd/atelet/main.go:prepareOCIBundles`: runtime namespace paths currently use
  worker UID only. Make them sandbox-specific and ensure generated OCI specs
  refer to the actual activation's namespace on both run and restore.
- Both ateom `main.go` files and runtime run/restore/checkpoint/shutdown code:
  replace worker-wide active session, network and cancellation state. MicroVM
  already has a UID-keyed `running` map, but its networking and stats remain
  single-actor. Update stats lookup by requested actor UID without blocking
  sampling on a long lifecycle operation.
- `cmd/ateom-microvm/net.go`: create and address the TAP in the gateway
  namespace, preserve guest/gateway MACs, and remove the TC cross-connect.
  Adapt boot/restore TAP FD provisioning and MTU discovery to this attachment.
- `internal/readyz`: support a session-specific route to the application.
- `internal/atunnel/{ingress,egress,client,credential}.go`: session dispatch,
  dial injection, independent credentials and teardown; preserve wire protocols.
- `internal/ateomcapacity/ateomcapacity.go`: replace the fixed capacity of one
  only after runtime support exists; account for shared proxy and per-sandbox
  runtime overhead instead of assigning the entire worker budget to each VM.

## Acceptance and rollout

Use real Linux namespaces and TCP, through `internal/roottest`, for the packet
path checks. Add runtime e2e coverage for the behavior namespace fixtures cannot
prove: gVisor packet delivery, direct TAP delivery, and VM snapshot restoration.

Required checks include same-address listeners; correct routing in both
directions; original destination and destination port; proxy loop prevention;
creation/cancellation/restoration errors; FD leak checks; independent teardown;
same-actor reactivation; stale generations; simultaneous credential renewal;
startup egress; worker process restart; and partial session setup rollback. Test HTTP,
gRPC and both supported CONNECT paths. Retain the fixed microVM MAC addresses
and verify a snapshot migrates between workers while another actor remains live.

Initially preserve the existing IPv4 actor-network scope. Reject unsupported
IPv6 actor-network configurations explicitly. Dual-stack actor networking needs
addressing, routing, NAT/capture, resolver and original-destination changes as
one separate deliverable; IPv6 listener support alone is insufficient.

Exercise the supported CNI configurations: namespace membership does not change
the proxy's cgroup membership, which can matter to cgroup-based eBPF policy and
service routing. Also test worker-pool NetworkPolicy and transit isolation.

Roll out opt-in to new workers, first at capacity one and then at capacity two.
Rollback drains/replaces affected workers before returning to the previous
topology. Measure total worker memory, connection throughput, activation
latency, and recovery time. Process count stays unchanged; additional namespace
and per-sandbox proxy state still cost memory. Register any
new metrics under the existing cardinality rules; actor IDs belong in logs
and traces. Run relevant unit/root/e2e tests and `make verify` before review.

This is a coordinated networking and runtime-lifecycle change. The namespace
socket helper is a small part; the critical work is making every network,
connection, credential and cleanup operation belong to one sandbox generation.
