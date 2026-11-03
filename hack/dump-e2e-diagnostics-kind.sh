#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit -o nounset -o pipefail

context="${KUBECTL_CONTEXT:-kind-${KIND_CLUSTER_NAME:-kind}}"
kubectl --context "${context}" get workerpool,pods -A -o wide || true

dump() {
  echo "=== logs: $1/$2 ==="
  kubectl --context "${context}" logs -n "$1" "$2" --all-containers --tail="$3" || true
}

for p in $(kubectl --context "${context}" get pods -n ate-system -o name); do
  dump ate-system "${p}" 300
done

# Failed tests keep their namespaces. Preserve all worker logs, including guest
# diagnostics emitted before a failed restore deletes the sandbox state.
kubectl --context "${context}" get pods -A -l ate.dev/worker-pool \
  -o 'custom-columns=:.metadata.namespace,:.metadata.name' --no-headers \
  | while read -r ns name; do dump "${ns}" "${name}" -1; done
