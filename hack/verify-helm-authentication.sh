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

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

rendered="$(helm template substrate charts/substrate \
  --namespace ate-system \
  --show-only templates/ate-api-authentication.yaml \
  --set-string auth.jwt.issuer=https://issuer.example)"

rg -q '^      issuer: "https://issuer\.example"$' <<<"${rendered}"
if rg -q 'certificateAuthorityFile|discoveryTokenFile' <<<"${rendered}"; then
  echo "external issuers must not use in-cluster discovery credentials" >&2
  exit 1
fi

rendered="$(helm template substrate charts/substrate \
  --namespace ate-system \
  --show-only templates/ate-api-authentication.yaml)"
if rg -q '^      issuer:' <<<"${rendered}"; then
  echo "default authentication config must omit the issuer" >&2
  exit 1
fi
rg -q '^      discoveryTokenFile: /var/run/secrets/kubernetes\.io/serviceaccount/token$' <<<"${rendered}"
