//go:build linux

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

package egress

import (
	"context"

	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// CapturePorts identifies the local listener ports used for transparent actor
// egress capture in the worker pod network namespace.
type CapturePorts struct {
	HTTP  uint16
	HTTPS uint16
}

// Gateway is a local egress gateway process that enforces an ActorTemplate
// egress policy for the currently active actor.
type Gateway interface {
	ApplyPolicy(ctx context.Context, defaultNamespace string, policy *ateompb.EgressPolicy) error
	Stop(ctx context.Context)
}

// Provider owns proxy-specific policy support decisions and gateway creation.
// Implementations can render different local proxy configs, such as
// AgentGateway or Envoy, without leaking those details into ateom service
// lifecycle and network setup code.
type Provider interface {
	Name() string
	CapturePorts() CapturePorts
	NeedsGateway(policy *ateompb.EgressPolicy) bool
	NewGateway(ctx context.Context) (Gateway, error)
}
