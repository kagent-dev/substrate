// Copyright 2026 Google LLC
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

package controlapi

import (
	"log/slog"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

const (
	sandboxClassGvisor  = "gvisor"
	sandboxClassMicroVM = "microvm"

	snapshotScopeFull = "Full"
	snapshotScopeData = "Data"

	actorTemplatePhaseInitial           = ""
	actorTemplatePhaseResumeGoldenActor = "ResumeGoldenActor"
	actorTemplatePhaseWaitGoldenActor   = "WaitGoldenActor"
	actorTemplatePhaseReady             = "Ready"
)

func toAteletSnapshotScope(in string) ateletpb.SnapshotScope {
	switch in {
	case snapshotScopeFull:
		return ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL
	case snapshotScopeData:
		return ateletpb.SnapshotScope_SNAPSHOT_SCOPE_DATA
	default:
		slog.Warn("unknown SnapshotScope; falling back to Full", "scope", in)
		return ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL
	}
}
