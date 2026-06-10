#!/usr/bin/env bash
# ateom-ch-kind-test.sh — end-to-end test for ateom-ch in a disposable kind cluster.
#
# What it does:
#   1. Creates a kind cluster with /dev/kvm + /dev/net/tun passed through.
#   2. Builds the ateom-ch Docker image (cloud-hypervisor + virtiofsd + kernel baked in).
#   3. Loads the image into kind.
#   4. Runs a privileged test pod.
#   5. Inside the pod: prepares an OCI bundle, runs ateom-ch, and exercises
#      RunWorkload → CheckpointWorkload → RestoreWorkload via grpcurl.
#
# PREREQUISITES:
#   - Docker
#   - kind  (https://kind.sigs.k8s.io)
#   - kubectl
#   - /dev/kvm available on the host (hardware virtualisation or nested virt)
#
# USAGE:
#   ./hack/ateom-ch-kind-test.sh [--keep]   # --keep: don't delete cluster on exit

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="ateom-ch-test"
IMAGE="ateom-ch:test"
POD="ateom-ch-test"
NAMESPACE="default"
KEEP_CLUSTER="${1:-}"

# ── Prerequisites ─────────────────────────────────────────────────────────────
for cmd in docker kind kubectl; do
    command -v "$cmd" &>/dev/null || { echo "ERROR: $cmd not found"; exit 1; }
done

if [[ ! -c /dev/kvm ]]; then
    echo "ERROR: /dev/kvm not found. Enable KVM (or nested virtualisation) on this host."
    exit 1
fi

# ── Cluster ───────────────────────────────────────────────────────────────────
cleanup() {
    local rc=$?
    if [[ "${KEEP_CLUSTER}" != "--keep" ]]; then
        echo ""
        echo "→ Deleting cluster ${CLUSTER}..."
        "${ROOT}/hack/kind.sh" delete cluster --name "${CLUSTER}" 2>/dev/null || true
    else
        echo "(cluster ${CLUSTER} kept — delete with: kind delete cluster --name ${CLUSTER})"
    fi
    exit $rc
}
trap cleanup EXIT

echo "→ Creating kind cluster '${CLUSTER}' with KVM + TUN access..."
cat > /tmp/ateom-ch-kind-config.yaml <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
  - hostPath: /dev/net/tun
    containerPath: /dev/net/tun
EOF

"${ROOT}/hack/kind.sh" delete cluster --name "${CLUSTER}" 2>/dev/null || true
"${ROOT}/hack/kind.sh" create cluster \
    --name "${CLUSTER}" \
    --config /tmp/ateom-ch-kind-config.yaml
# kind already merged the context into ~/.kube/config on cluster creation.
export KUBECONFIG="${HOME}/.kube/config"

# Verify /dev/kvm is visible inside the kind node.
KINDNODE="$( "${ROOT}/hack/kind.sh" get nodes --name "${CLUSTER}" | head -1 )"
if ! docker exec "${KINDNODE}" test -c /dev/kvm; then
    echo "ERROR: /dev/kvm not visible inside kind node ${KINDNODE}."
    echo "  Your Docker daemon may not pass device files via bind mounts."
    echo "  Try: docker run --rm --device /dev/kvm ubuntu ls -l /dev/kvm"
    exit 1
fi
echo "  /dev/kvm confirmed inside kind node."

# ── Build image ───────────────────────────────────────────────────────────────
echo ""
echo "→ Building ateom-ch Docker image (this downloads ~200 MB of assets)..."
docker build \
    --file "${ROOT}/build/ateom-ch/Dockerfile" \
    --tag "${IMAGE}" \
    "${ROOT}"
echo "  Image built: ${IMAGE}"

echo "→ Loading image into kind cluster..."
"${ROOT}/hack/kind.sh" load docker-image "${IMAGE}" --name "${CLUSTER}"

# ── Test pod ──────────────────────────────────────────────────────────────────
echo ""
echo "→ Launching test pod '${POD}'..."
kubectl --context "kind-${CLUSTER}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: ${NAMESPACE}
spec:
  restartPolicy: Never
  containers:
  - name: ateom-ch
    image: ${IMAGE}
    imagePullPolicy: Never
    # Sleep until the test script execs into the pod.
    command: ["sleep", "infinity"]
    securityContext:
      privileged: true
      runAsUser: 0
    volumeMounts:
    - name: ateom-state
      mountPath: /var/lib/ateom-gvisor
    - name: ateom-run
      mountPath: /run/ateom-ch
  volumes:
  - name: ateom-state
    emptyDir: {}
  - name: ateom-run
    emptyDir: {}
EOF

echo "→ Waiting for pod to be Running..."
kubectl --context "kind-${CLUSTER}" wait pod "${POD}" \
    --namespace "${NAMESPACE}" \
    --for=condition=Ready \
    --timeout=120s

# ── In-pod test ───────────────────────────────────────────────────────────────
# Everything from here runs inside the pod via kubectl exec.
# We pass a heredoc as a shell script so the whole test is one exec call.

POD_UID="test-pod-$(date +%s)"
NS="testns"
TMPL="testtmpl"
ACTOR="actor1"
CONTAINER="c0"

BUNDLE_DIR="/var/lib/ateom-gvisor/actors/${NS}:${TMPL}:${ACTOR}/bundles/${CONTAINER}"
ROOTFS_DIR="${BUNDLE_DIR}/rootfs"
SOCKET="/var/lib/ateom-gvisor/ateoms/${POD_UID}/ateom.sock"
CHECKPOINT_DIR="/var/lib/ateom-gvisor/actors/${NS}:${TMPL}:${ACTOR}/checkpoint-state"
RESTORE_DIR="/var/lib/ateom-gvisor/actors/${NS}:${TMPL}:${ACTOR}/restore-state"

echo ""
echo "→ Running ateom-ch lifecycle test inside pod..."

# Write test script to a tmpfile, copy into pod, then execute.
# (kubectl exec -- bash -s <<HEREDOC doesn't forward stdin reliably in scripts.)
TMPSCRIPT="$(mktemp /tmp/ateom-ch-test-XXXX.sh)"
trap 'rm -f "${TMPSCRIPT}"' EXIT

cat > "${TMPSCRIPT}" <<SCRIPT

set -euo pipefail

BUNDLE_DIR="${BUNDLE_DIR}"
ROOTFS_DIR="${ROOTFS_DIR}"
SOCKET="${SOCKET}"
CHECKPOINT_DIR="${CHECKPOINT_DIR}"
RESTORE_DIR="${RESTORE_DIR}"
NS="${NS}"
TMPL="${TMPL}"
ACTOR="${ACTOR}"
CONTAINER="${CONTAINER}"
POD_UID="${POD_UID}"

section() { echo ""; echo "── \$1 ──────────────────────────────────────"; }

# ── 1. OCI bundle ────────────────────────────────────────────────────────────
section "Preparing OCI bundle"

mkdir -p "\${ROOTFS_DIR}"

# Use the baked-in testinit binary as the guest's PID 1.
cp /usr/local/share/ateom-ch/testinit "\${ROOTFS_DIR}/init"
chmod +x "\${ROOTFS_DIR}/init"

# Minimal rootfs: testinit only needs itself and a couple of /proc entries.
# The kernel mounts virtiofs as rootfs before exec-ing /init, so no libc needed.
cat > "\${BUNDLE_DIR}/config.json" <<'JSON'
{
  "process": {
    "args": ["/init"],
    "cwd": "/"
  },
  "root": { "path": "rootfs" }
}
JSON

echo "  Bundle: \${BUNDLE_DIR}"
echo "  Rootfs files: \$(ls \${ROOTFS_DIR})"

# ── 2. Start ateom-ch ────────────────────────────────────────────────────────
section "Starting ateom-ch"

mkdir -p "\$(dirname "\${SOCKET}")"
ateom-ch --pod-uid "\${POD_UID}" &
ATEOM_PID=\$!
echo "  ateom-ch PID: \${ATEOM_PID}"

# Wait for unix socket.
echo -n "  Waiting for gRPC socket"
for i in \$(seq 1 60); do
    [[ -S "\${SOCKET}" ]] && break
    echo -n "."
    sleep 0.5
done
echo ""
[[ -S "\${SOCKET}" ]] || { echo "ERROR: socket not created after 30s"; exit 1; }
echo "  Socket ready: \${SOCKET}"

grpc() {
    local method="\$1" body="\$2"
    grpcurl -plaintext -unix -d "\${body}" "\${SOCKET}" "ateom.Ateom/\${method}"
}

# ── 3. RunWorkload ────────────────────────────────────────────────────────────
section "RunWorkload"

T0=\$(date +%s%3N)
grpc RunWorkload "{
  \"actor_template_namespace\": \"\${NS}\",
  \"actor_template_name\":      \"\${TMPL}\",
  \"actor_id\":                 \"\${ACTOR}\",
  \"spec\": { \"containers\": [{ \"name\": \"\${CONTAINER}\" }] }
}"
T1=\$(date +%s%3N)
echo "  RunWorkload: \$((T1 - T0))ms"

echo "  Waiting 5s for VM to settle..."
sleep 5

# ── 4. CheckpointWorkload ─────────────────────────────────────────────────────
section "CheckpointWorkload"

T0=\$(date +%s%3N)
grpc CheckpointWorkload "{
  \"actor_template_namespace\": \"\${NS}\",
  \"actor_template_name\":      \"\${TMPL}\",
  \"actor_id\":                 \"\${ACTOR}\",
  \"spec\": { \"containers\": [{ \"name\": \"\${CONTAINER}\" }] },
  \"snapshot_uri_prefix\": \"file://\${CHECKPOINT_DIR}\"
}"
T1=\$(date +%s%3N)
echo "  CheckpointWorkload: \$((T1 - T0))ms"

echo ""
echo "  Snapshot files:"
ls -lh "\${CHECKPOINT_DIR}/" 2>/dev/null || echo "  (empty — unexpected)"
SNAP_BYTES=\$(du -sb "\${CHECKPOINT_DIR}" 2>/dev/null | cut -f1 || echo 0)
echo "  Total: \$(( SNAP_BYTES / 1024 / 1024 ))MB"

# ── 5. RestoreWorkload ────────────────────────────────────────────────────────
section "RestoreWorkload"

# Simulate atelet: copy checkpoint → restore dir.
cp -r "\${CHECKPOINT_DIR}/." "\${RESTORE_DIR}/"
echo "  Snapshot copied to restore dir."

T0=\$(date +%s%3N)
grpc RestoreWorkload "{
  \"actor_template_namespace\": \"\${NS}\",
  \"actor_template_name\":      \"\${TMPL}\",
  \"actor_id\":                 \"\${ACTOR}\",
  \"spec\": { \"containers\": [{ \"name\": \"\${CONTAINER}\" }] },
  \"snapshot_uri_prefix\": \"file://\${RESTORE_DIR}\"
}"
T1=\$(date +%s%3N)
echo "  RestoreWorkload: \$((T1 - T0))ms"

echo ""
echo "  Waiting 3s to verify restored VM is alive..."
sleep 3

# Final checkpoint to confirm restore works end-to-end.
grpc CheckpointWorkload "{
  \"actor_template_namespace\": \"\${NS}\",
  \"actor_template_name\":      \"\${TMPL}\",
  \"actor_id\":                 \"\${ACTOR}\",
  \"spec\": { \"containers\": [{ \"name\": \"\${CONTAINER}\" }] },
  \"snapshot_uri_prefix\": \"file://\${CHECKPOINT_DIR}\"
}"
echo "  Second checkpoint succeeded — restored VM is healthy."

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────────"
echo "PASS: ateom-ch RunWorkload → Checkpoint → Restore → Checkpoint"
echo "────────────────────────────────────────────────"

SCRIPT

kubectl --context "kind-${CLUSTER}" cp \
    "${TMPSCRIPT}" "${NAMESPACE}/${POD}:/tmp/ateom-ch-test.sh"

# kubectl exec can return non-zero due to WebSocket stream teardown noise even
# when the remote script exits 0. Stream output into a tmpfile so we can check
# for the PASS marker regardless of kubectl's own exit code.
_out="$(mktemp)"
kubectl --context "kind-${CLUSTER}" exec "${POD}" \
    --namespace "${NAMESPACE}" \
    -- bash -euo pipefail /tmp/ateom-ch-test.sh \
    2>/dev/null | tee "${_out}" || true
if ! grep -q "^PASS:" "${_out}"; then
  rm -f "${_out}"
  echo "" >&2
  echo "FAIL: test script did not print PASS marker" >&2
  exit 1
fi
rm -f "${_out}"

echo ""
echo "Test passed."
