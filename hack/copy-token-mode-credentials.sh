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

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 NAMESPACE" >&2
  exit 1
fi

namespace="$1"
context_args=()
if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
  context_args+=(--context="${KUBECTL_CONTEXT}")
fi

# The source only exists in token mode; certificate mode needs no copy.
if ! kubectl "${context_args[@]}" get secret token-mode-tls -n ate-system >/dev/null 2>&1; then
  exit 0
fi

credential_dir=$(mktemp -d)
trap 'rm -r -- "${credential_dir}"' EXIT

kubectl "${context_args[@]}" get secret token-mode-tls -n ate-system -o jsonpath='{.data.credential-bundle\.pem}' | base64 --decode >"${credential_dir}/credential-bundle.pem"
kubectl "${context_args[@]}" get secret token-mode-tls -n ate-system -o jsonpath='{.data.tls\.crt}' | base64 --decode >"${credential_dir}/tls.crt"
kubectl "${context_args[@]}" get secret token-mode-tls -n ate-system -o jsonpath='{.data.tls\.key}' | base64 --decode >"${credential_dir}/tls.key"
kubectl "${context_args[@]}" get configmap token-mode-ca -n ate-system -o jsonpath='{.data.ca\.crt}' >"${credential_dir}/ca.crt"
kubectl "${context_args[@]}" get configmap token-mode-ca -n ate-system -o jsonpath='{.data.trust-bundle\.pem}' >"${credential_dir}/trust-bundle.pem"

kubectl "${context_args[@]}" create secret generic token-mode-tls -n "${namespace}" \
  --from-file="${credential_dir}/credential-bundle.pem" --from-file="${credential_dir}/tls.crt" --from-file="${credential_dir}/tls.key" \
  --dry-run=client -o yaml | kubectl "${context_args[@]}" apply -f -
kubectl "${context_args[@]}" create configmap token-mode-ca -n "${namespace}" \
  --from-file="${credential_dir}/ca.crt" --from-file="${credential_dir}/trust-bundle.pem" \
  --dry-run=client -o yaml | kubectl "${context_args[@]}" apply -f -
