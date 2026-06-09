#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Install Agent Substrate on a kind cluster in JWT auth mode.
#
# Unlike the mTLS install path (hack/install-ate-kind.sh), this works on a
# stock Kubernetes cluster — no ClusterTrustBundle / PodCertificateRequest
# feature gates required. Suitable for a kind cluster created with
# KIND_ENABLE_PODCERT=false hack/create-kind-cluster.sh.
#
# Steps:
#   1. Bootstrap a self-signed Secret/ateapi-tls + ConfigMap/ateapi-ca via
#      openssl (the chart references these but expects them to exist
#      out-of-band).
#   2. Render the chart with auth.mode=jwt + kind-specific values, resolve
#      ko:// image refs against a local registry, and apply.
#   3. Apply the kind-only extras (rustfs storage, OTel collector) from
#      manifests/ate-install/kind/.
set -o errexit -o nounset -o pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS="${NS:-ate-system}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-}"
KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/$(go env GOARCH)}"
reg_name="kind-registry"
reg_port="5001"

export KO_DOCKER_REPO KO_DEFAULTPLATFORMS

run_kubectl() {
  kubectl ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} "$@"
}

log_step() {
  echo -e "\033[1;36m[step]:\033[0m $1"
}

ensure_namespace() {
  log_step "ensure_namespace ${NS}"
  run_kubectl create namespace "${NS}" --dry-run=client -o yaml | run_kubectl apply -f -
}

ensure_kind_local_registry() {
  log_step "ensure_kind_local_registry"

  if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" == "true" ]; then
    if ! docker port "${reg_name}" | grep -q "${reg_port}"; then
      echo "Registry exists but is not mapped to port ${reg_port}. Recreating..."
      docker rm -f "${reg_name}"
    fi
  fi

  if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != "true" ]; then
    docker run \
      -d --restart=always \
      --label created-by=agent-substrate \
      -p "127.0.0.1:${reg_port}:5000" \
      -p "[::1]:${reg_port}:5000" \
      --network bridge --name "${reg_name}" \
      registry:3
  fi

  if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = "null" ]; then
    docker network connect "kind" "${reg_name}"
  fi

  local registry_dir="/etc/containerd/certs.d/localhost:${reg_port}"
  local node
  for node in $("${ROOT}"/hack/kind.sh get nodes --name "${KIND_CLUSTER_NAME}"); do
    docker exec "${node}" mkdir -p "${registry_dir}"
    cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${registry_dir}/hosts.toml"
[host."http://${reg_name}:5000"]
EOF
  done

  cat <<EOF | run_kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${reg_port}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
}

# Generate a self-signed CA + server cert pair and install them as the
# Secret/ConfigMap pair the chart references. Idempotent: skips if both
# resources already exist.
bootstrap_jwt_tls() {
  log_step "bootstrap_jwt_tls"
  if run_kubectl get secret -n "${NS}" ateapi-tls >/dev/null 2>&1 \
     && run_kubectl get configmap -n "${NS}" ateapi-ca >/dev/null 2>&1; then
    echo "Secret/ateapi-tls and ConfigMap/ateapi-ca already present — skipping."
    return
  fi

  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN

  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj "/CN=ateapi-ca" \
    -keyout "${tmp}/ca.key" -out "${tmp}/ca.crt" >/dev/null 2>&1

  openssl req -newkey rsa:2048 -nodes \
    -subj "/CN=api.${NS}.svc" \
    -keyout "${tmp}/server.key" -out "${tmp}/server.csr" >/dev/null 2>&1

  cat > "${tmp}/server.ext" <<EOF
subjectAltName = DNS:api.${NS}.svc,DNS:api.${NS}.svc.cluster.local,DNS:atenet-router.${NS}.svc
EOF
  openssl x509 -req -in "${tmp}/server.csr" -CA "${tmp}/ca.crt" -CAkey "${tmp}/ca.key" \
    -CAcreateserial -out "${tmp}/server.crt" -days 365 \
    -extfile "${tmp}/server.ext" >/dev/null 2>&1

  run_kubectl -n "${NS}" create secret tls ateapi-tls \
    --cert="${tmp}/server.crt" --key="${tmp}/server.key" \
    --dry-run=client -o yaml | run_kubectl apply -f -

  run_kubectl -n "${NS}" create configmap ateapi-ca \
    --from-file=ca.crt="${tmp}/ca.crt" \
    --dry-run=client -o yaml | run_kubectl apply -f -
}

apply_chart() {
  log_step "apply_chart (helm template | ko resolve | kubectl apply)"
  local rendered
  rendered=$(helm template substrate "${ROOT}/charts/substrate" \
    --namespace "${NS}" \
    -f "${ROOT}/hack/values-kind-jwt.yaml" \
    --set 'image.tag=<none>')

  # ko resolve replaces ko:// refs with built+pushed image refs.
  echo "${rendered}" | bash "${ROOT}/hack/run-tool.sh" ko resolve -f - \
    | run_kubectl apply -f -
}

apply_crds() {
  log_step "apply_crds"
  run_kubectl apply -f "${ROOT}/manifests/ate-install/generated"
}

bootstrap_session_id_secrets() {
  log_step "bootstrap_session_id_secrets"
  run_kubectl get secret -n "${NS}" session-id-jwt-pool >/dev/null 2>&1 \
    || kubectl-ate admin make-jwt-pool --key-id="1" --name="session-id-jwt-pool" --secret-namespace="${NS}"
  run_kubectl get secret -n "${NS}" session-id-ca-pool >/dev/null 2>&1 \
    || kubectl-ate admin make-ca-pool --ca-id="1" --name="session-id-ca-pool" --secret-namespace="${NS}"
}

bootstrap_envvars_configmap() {
  log_step "bootstrap_envvars_configmap"
  run_kubectl get configmap -n "${NS}" ate-api-server-envvars >/dev/null 2>&1 && return
  run_kubectl create configmap -n "${NS}" ate-api-server-envvars \
    --from-literal=ATE_API_REDIS_ADDRESS="valkey-cluster.${NS}.svc:6379" \
    --from-literal=ATE_API_REDIS_USE_IAM_AUTH="false" \
    --from-literal=ATE_API_REDIS_TLS_SERVER_NAME="" \
    --from-literal=ATE_API_REDIS_CLIENT_CERT="" \
    --dry-run=client -o yaml | run_kubectl apply -f -
}

apply_kind_extras() {
  log_step "apply_kind_extras (rustfs + otel-collector)"
  run_kubectl apply -f "${ROOT}/manifests/ate-install/kind/rustfs.yaml"
  run_kubectl apply -f "${ROOT}/manifests/ate-install/kind/otel-collector.yaml"
}

wait_rollouts() {
  log_step "wait_rollouts"
  run_kubectl -n "${NS}" rollout status deployment/ate-api-server-deployment --timeout=180s
  run_kubectl -n "${NS}" rollout status deployment/ate-controller --timeout=180s
  run_kubectl -n "${NS}" rollout status deployment/atenet-router --timeout=180s
  run_kubectl -n "${NS}" rollout status daemonset/atelet --timeout=180s
  run_kubectl -n "${NS}" rollout status statefulset/valkey-cluster --timeout=180s
}

ensure_namespace
ensure_kind_local_registry
apply_crds
bootstrap_jwt_tls
bootstrap_session_id_secrets
bootstrap_envvars_configmap
apply_chart
apply_kind_extras
wait_rollouts

echo "Substrate (JWT mode) installed in namespace ${NS}."
