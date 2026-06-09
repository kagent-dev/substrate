#!/usr/bin/env bash
# ch-snapshot-test.sh — start a Cloud Hypervisor VM, pause it, snapshot it,
# then stream the snapshot files to a local HTTP receiver.
#
# Cloud Hypervisor's snapshot API writes to a local directory (file:// URL).
# This script snapshots locally then uploads each file to an HTTP server,
# simulating what GCS/S3 streaming would look like from atelet.
#
# USAGE:
#   KERNEL=/path/to/kernel DISK=/path/to/rootfs.img ./hack/ch-snapshot-test.sh
#
# ASSETS (if you don't have them):
#   ./hack/ch-snapshot-test.sh --download-assets
#   This fetches a minimal Alpine Linux kernel + cloud image into /tmp/ch-test-assets/
#
# DEPENDENCIES:
#   cloud-hypervisor   https://github.com/cloud-hypervisor/cloud-hypervisor/releases
#   curl, python3

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
CH_BIN="${CLOUD_HYPERVISOR:-cloud-hypervisor}"
HTTP_PORT="${HTTP_PORT:-8765}"
MEMORY_MB="${MEMORY_MB:-512}"
VCPUS="${VCPUS:-1}"
ASSETS_DIR="${ASSETS_DIR:-/tmp/ch-test-assets}"

# ── Download assets mode ──────────────────────────────────────────────────────
if [[ "${1:-}" == "--download-assets" ]]; then
    mkdir -p "${ASSETS_DIR}"
    echo "→ Downloading Alpine Linux cloud image (raw)..."
    # Alpine nocloud image: small (~50MB), boots directly with an extracted kernel.
    ALPINE_VERSION="3.21.3"
    ALPINE_IMG="alpine-virt-${ALPINE_VERSION}-x86_64.iso"
    ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/${ALPINE_IMG}"
    if [[ ! -f "${ASSETS_DIR}/${ALPINE_IMG}" ]]; then
        curl -L --progress-bar -o "${ASSETS_DIR}/${ALPINE_IMG}" "${ALPINE_URL}"
    else
        echo "  (already downloaded)"
    fi

    echo ""
    echo "→ Downloading Cloud Hypervisor test kernel..."
    # Cloud Hypervisor CI kernel — built with cloud-hypervisor defconfig.
    # These are the kernels their own integration tests use.
    CH_KERNEL_URL="https://github.com/cloud-hypervisor/linux/releases/download/ch-6.1.95-1/vmlinux"
    if [[ ! -f "${ASSETS_DIR}/vmlinux" ]]; then
        curl -L --progress-bar -o "${ASSETS_DIR}/vmlinux" "${CH_KERNEL_URL}"
    else
        echo "  (already downloaded)"
    fi

    echo ""
    echo "Assets ready in ${ASSETS_DIR}:"
    ls -lh "${ASSETS_DIR}/"
    echo ""
    echo "Run the test with:"
    echo "  KERNEL=${ASSETS_DIR}/vmlinux DISK=${ASSETS_DIR}/${ALPINE_IMG} ./hack/ch-snapshot-test.sh"
    exit 0
fi

# ── Validate inputs ───────────────────────────────────────────────────────────
: "${KERNEL:?Set KERNEL= path to Linux kernel (vmlinux or bzImage)}"
: "${DISK:?Set DISK= path to rootfs disk image}"
[[ -f "${KERNEL}" ]] || { echo "ERROR: kernel not found: ${KERNEL}"; exit 1; }
[[ -f "${DISK}" ]]   || { echo "ERROR: disk not found: ${DISK}"; exit 1; }
command -v "${CH_BIN}" &>/dev/null || { echo "ERROR: ${CH_BIN} not found. Set CLOUD_HYPERVISOR= or install it."; exit 1; }

echo "cloud-hypervisor: $(${CH_BIN} --version 2>&1 | head -1)"
echo "kernel:           ${KERNEL} ($(du -h "${KERNEL}" | cut -f1))"
echo "disk:             ${DISK} ($(du -h "${DISK}" | cut -f1))"
echo "memory:           ${MEMORY_MB}MB"
echo ""

# ── Temp workspace ────────────────────────────────────────────────────────────
WORK="$(mktemp -d /tmp/ch-snapshot-test-XXXX)"
API_SOCK="${WORK}/api.sock"
SNAPSHOT_DIR="${WORK}/snapshot"
RECEIVED_DIR="${WORK}/received"
SERIAL_LOG="${WORK}/serial.log"
CH_LOG="${WORK}/ch.log"
HTTP_LOG="${WORK}/http.log"

mkdir -p "${SNAPSHOT_DIR}" "${RECEIVED_DIR}"

CH_PID=""
HTTP_PID=""

cleanup() {
    local rc=$?
    echo ""
    echo "→ Cleanup..."
    [[ -n "${CH_PID}" ]] && kill "${CH_PID}" 2>/dev/null || true
    [[ -n "${HTTP_PID}" ]] && kill "${HTTP_PID}" 2>/dev/null || true
    wait 2>/dev/null || true
    if [[ $rc -ne 0 ]]; then
        echo ""
        echo "── cloud-hypervisor log (last 30 lines) ──"
        tail -30 "${CH_LOG}" 2>/dev/null || true
        echo ""
        echo "── serial output ──"
        cat "${SERIAL_LOG}" 2>/dev/null || true
    fi
    rm -rf "${WORK}"
}
trap cleanup EXIT

# ── HTTP receiver ─────────────────────────────────────────────────────────────
# Accepts PUT /path → saves file under RECEIVED_DIR, prints size + timing.
python3 - <<PYEOF >"${HTTP_LOG}" 2>&1 &
import http.server, os, time, sys

RECEIVED = "${RECEIVED_DIR}"

class Handler(http.server.BaseHTTPRequestHandler):
    def do_PUT(self):
        rel = self.path.lstrip("/")
        dest = os.path.join(RECEIVED, rel)
        os.makedirs(os.path.dirname(dest) or RECEIVED, exist_ok=True)
        n = int(self.headers.get("Content-Length", 0))
        t0 = time.monotonic()
        data = self.rfile.read(n)
        elapsed_ms = (time.monotonic() - t0) * 1000
        with open(dest, "wb") as f:
            f.write(data)
        mb = n / 1024 / 1024
        print(f"  PUT /{rel}  {mb:.1f} MB  {elapsed_ms:.0f}ms  ({mb/(elapsed_ms/1000):.0f} MB/s)", flush=True)
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass

httpd = http.server.HTTPServer(("127.0.0.1", ${HTTP_PORT}), Handler)
print(f"listening on 127.0.0.1:${HTTP_PORT}", flush=True)
httpd.serve_forever()
PYEOF
HTTP_PID=$!

# Wait for Python server to start
sleep 0.3
kill -0 "${HTTP_PID}" 2>/dev/null || { echo "ERROR: HTTP server failed to start"; cat "${HTTP_LOG}"; exit 1; }
echo "→ HTTP receiver on port ${HTTP_PORT} (PID ${HTTP_PID})"

# ── Start Cloud Hypervisor ────────────────────────────────────────────────────
echo "→ Starting cloud-hypervisor..."
"${CH_BIN}" --api-socket "${API_SOCK}" >"${CH_LOG}" 2>&1 &
CH_PID=$!

# Wait for API socket
for i in $(seq 1 50); do
    [[ -S "${API_SOCK}" ]] && break
    sleep 0.1
done
[[ -S "${API_SOCK}" ]] || { echo "ERROR: API socket not created after 5s"; exit 1; }

api() {
    local method="$1" path="$2"
    shift 2
    curl -sf --unix-socket "${API_SOCK}" -X "${method}" "http://localhost${path}" "$@"
}

PING=$(api GET /api/v1/vmm.ping)
echo "  ping: ${PING}"

# ── Create VM ─────────────────────────────────────────────────────────────────
echo "→ Creating VM..."
api PUT /api/v1/vm.create \
    -H 'Content-Type: application/json' \
    -d "{
      \"payload\": {
        \"kernel\": \"${KERNEL}\",
        \"cmdline\": \"console=ttyS0 root=/dev/vda rw panic=1 quiet\"
      },
      \"disks\": [{\"path\": \"${DISK}\", \"readonly\": false}],
      \"cpus\":   {\"boot_vcpus\": ${VCPUS}, \"max_vcpus\": ${VCPUS}},
      \"memory\": {\"size\": $((MEMORY_MB * 1024 * 1024))},
      \"serial\": {\"mode\": \"File\", \"file\": \"${SERIAL_LOG}\"},
      \"console\": {\"mode\": \"Off\"}
    }"

echo "→ Booting..."
api PUT /api/v1/vm.boot
echo "  booted. Waiting ${BOOT_WAIT_SEC:-8}s for guest to settle..."
sleep "${BOOT_WAIT_SEC:-8}"

echo ""
echo "── serial (last 10 lines) ──"
tail -10 "${SERIAL_LOG}" 2>/dev/null || echo "  (empty)"
echo "────────────────────────────"
echo ""

# ── Pause ─────────────────────────────────────────────────────────────────────
echo "→ Pausing VM..."
T0=$(date +%s%3N)
api PUT /api/v1/vm.pause
T1=$(date +%s%3N)
echo "  pause: $((T1 - T0))ms"

VM_INFO=$(api GET /api/v1/vm.info)
echo "  state: $(echo "${VM_INFO}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"

# ── Snapshot ──────────────────────────────────────────────────────────────────
echo "→ Snapshotting to ${SNAPSHOT_DIR}..."
T0=$(date +%s%3N)
api PUT /api/v1/vm.snapshot \
    -H 'Content-Type: application/json' \
    -d "{\"destination_url\": \"file://${SNAPSHOT_DIR}\"}"
T1=$(date +%s%3N)
SNAPSHOT_MS=$((T1 - T0))
echo "  snapshot: ${SNAPSHOT_MS}ms"
echo ""

echo "── snapshot files ──"
ls -lh "${SNAPSHOT_DIR}/"
TOTAL_BYTES=$(du -sb "${SNAPSHOT_DIR}" | cut -f1)
echo "total: $(( TOTAL_BYTES / 1024 / 1024 ))MB"
echo "────────────────────"
echo ""

# ── Upload to HTTP server ─────────────────────────────────────────────────────
echo "→ Uploading snapshot to HTTP server (port ${HTTP_PORT})..."
echo ""
T0=$(date +%s%3N)
for f in "${SNAPSHOT_DIR}"/*; do
    fname="$(basename "${f}")"
    curl -sf -X PUT \
        -H "Content-Type: application/octet-stream" \
        --data-binary "@${f}" \
        "http://127.0.0.1:${HTTP_PORT}/snapshot/${fname}"
done
T1=$(date +%s%3N)
UPLOAD_MS=$((T1 - T0))
echo ""

echo "── received by HTTP server ──"
ls -lh "${RECEIVED_DIR}/snapshot/" 2>/dev/null || echo "  (empty)"
echo "─────────────────────────────"
echo ""

# ── Summary ───────────────────────────────────────────────────────────────────
echo "── timings ──────────────────"
echo "  pause:    $((T1 - T0)) ... (see above)"
echo "  snapshot: ${SNAPSHOT_MS}ms"
echo "  upload:   ${UPLOAD_MS}ms  (loopback — scale for GCS latency)"
echo "  total:    $((SNAPSHOT_MS + UPLOAD_MS))ms"
echo ""
echo "  snapshot size: $(( TOTAL_BYTES / 1024 / 1024 ))MB  (= guest RAM + VM state)"
echo "─────────────────────────────"
echo ""
echo "✓ Done. Snapshot is in ${RECEIVED_DIR}/snapshot/ before cleanup."
echo "  (will be deleted when script exits — copy if needed)"
