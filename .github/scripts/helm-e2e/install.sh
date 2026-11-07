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

# Networking tests assert the debug-level CONNECT tunnel log.
bin/kubectl-ate install \
  --chart-dir ./charts \
  --timeout 10m \
  --microvm \
  -f internal/e2e/suites/credentials/values.yaml \
  -f internal/e2e/suites/parking/values.yaml \
  --set image.registry=localhost:5001 \
  --set image.tag="${VERSION}" \
  --set 'preview[0]=ALL' \
  --set 'atenetRouter.ingressAllowedSPIFFEIDs[0]=spiffe://cluster.local/ns/ate-e2e-router-client/sa/router-client' \
  --set atenetEgress.agentgateway.logLevel=debug \
  --set 'atelet.extraArgs[0]=--localhost-registry-replacement=kind-registry:5000' \
  --set 'atelet.extraArgs[1]=--snapshot-upload-delay=2s' \
  --set 'atelet.extraArgs[2]=--snapshot-detach-delay=30s' \
  --set otel.endpoint=http://opentelemetry-collector.otel-system.svc:4317

bin/kubectl-ate status
