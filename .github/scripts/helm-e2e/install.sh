#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
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

# Install Agent Substrate on kind with kubectl ate install, from the charts in
# this checkout and the images "Build chart images" pushed.
set -o errexit -o nounset -o pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}"

go build -o bin/kubectl-ate ./cmd/kubectl-ate

# valuesFiles resolve against the current directory for a config on stdin.
bin/kubectl-ate install --chart-dir ./charts -c - <<EOF
timeout: 10m
microvm:
  enabled: true
releases:
  all:
    valuesFiles:
    - internal/e2e/suites/credentials/values.yaml
    - internal/e2e/suites/parking/values.yaml
    values:
      image:
        registry: localhost:5001
        tag: "${VERSION}"
      preview: [ALL]
      atenetRouter:
        ingressAllowedSPIFFEIDs:
        - spiffe://cluster.local/ns/ate-e2e-router-client/sa/router-client
      atenetEgress:
        # Networking tests assert the debug-level CONNECT tunnel log.
        agentgateway:
          logLevel: debug
      atelet:
        extraArgs:
        - --localhost-registry-replacement=kind-registry:5000
        - --snapshot-upload-delay=2s
        - --snapshot-detach-delay=30s
      otel:
        endpoint: http://opentelemetry-collector.otel-system.svc:4317
EOF

bin/kubectl-ate status
