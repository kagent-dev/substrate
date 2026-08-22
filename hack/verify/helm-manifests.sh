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
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

helm template substrate "${ROOT}/charts/substrate" \
  --namespace ate-system \
  --set image.registry=ko://github.com/agent-substrate/substrate/cmd \
  --set image.tag="<none>" \
  >"${TMP_DIR}/chart.yaml"

kubectl kustomize "${ROOT}/manifests/ate-install/agentgateway-egress" \
  --load-restrictor LoadRestrictionsNone >"${TMP_DIR}/manifests.yaml"

python3 - "${TMP_DIR}/chart.yaml" "${TMP_DIR}/manifests.yaml" <<'PY'
import difflib
import re
import sys

import yaml

chart_path, manifests_path = sys.argv[1:]
template = "atenet-egress.yaml"


def normalize(value):
    if isinstance(value, dict):
        return {key: normalize(item) for key, item in value.items()}
    if isinstance(value, list):
        return [normalize(item) for item in value]
    if isinstance(value, str):
        return value.rstrip("\n")
    return value

with open(chart_path) as stream:
    rendered = stream.read()

chart = {}
for document in rendered.split("\n---\n"):
    source = re.search(r"^# Source: [^/]+/templates/(.+)$", document, re.MULTILINE)
    if not source or source.group(1) != template:
        continue
    resource = yaml.safe_load(document)
    key = (resource["apiVersion"], resource["kind"], resource["metadata"]["name"])
    chart[key] = normalize(resource)

with open(manifests_path) as stream:
    resources = [resource for resource in yaml.safe_load_all(stream) if resource]
manifests = {
    (resource["apiVersion"], resource["kind"], resource["metadata"]["name"]): normalize(resource)
    for resource in resources
}

if not chart:
    raise SystemExit(f"Helm template {template} rendered no resources")

if chart.keys() != manifests.keys():
    missing = sorted(manifests.keys() - chart.keys())
    extra = sorted(chart.keys() - manifests.keys())
    raise SystemExit(f"Helm/manifests resource mismatch; missing from Helm: {missing}; only in Helm: {extra}")

for key in sorted(chart):
    if chart[key] == manifests[key]:
        continue
    expected = yaml.safe_dump(manifests[key], sort_keys=True).splitlines()
    actual = yaml.safe_dump(chart[key], sort_keys=True).splitlines()
    print("\n".join(difflib.unified_diff(expected, actual, "manifests", "Helm")), file=sys.stderr)
    raise SystemExit(f"Helm resource is out of sync with manifests: {'/'.join(key)}")

print(f"Helm {template} matches the AgentGateway manifest overlay.")
PY
