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

// Package charts embeds the Helm charts so kubectl ate install needs no
// checkout or chart registry.
package charts

import "embed"

// FS holds one directory per chart. The all: prefix keeps files starting
// with _ or ., such as _helpers.tpl.
//
//go:embed all:substrate all:substrate-atelet all:substrate-crds all:substrate-podcert
var FS embed.FS
