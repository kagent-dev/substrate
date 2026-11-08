// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ateinstall embeds install manifests that kubectl ate applies.
package ateinstall

import "embed"

// Postgres holds the evaluation PostgreSQL manifest and the kind overlay that
// sizes it down. Build kind/postgres to apply it.
//
//go:embed postgres/postgres.yaml kind/postgres/kustomization.yaml
var Postgres embed.FS

// MicroVMSandboxConfig is the microvm SandboxConfig. ${BUCKET_SCHEME} and
// ${ASSET_BUCKET_NAME} name the bucket its assets are staged in.
//
//go:embed sandboxconfig-microvm.yaml.tmpl
var MicroVMSandboxConfig []byte
