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
	"context"
	"fmt"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// resolveSandboxAssets determines the sandbox binaries an actor should boot with
// and projects them onto the ateletpb.SandboxAssets atelet fetches. It takes the
// SandboxClass (default gvisor) of a given worker pool, then picks the SandboxConfig
// named by the pool — or, if none is named, the cluster default SandboxConfig for that class.
func resolveSandboxAssets(
	ctx context.Context,
	st store.Interface,
	poolNamespace, poolName string,
) (*ateletpb.SandboxAssets, error) {
	protoWP, err := st.GetWorkerPool(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("while getting WorkerPool %s: %w", poolName, err)
	}

	class := protoWP.GetSpec().GetSandboxClass()
	if class == "" {
		class = sandboxClassGvisor
	}

	var sc *ateapipb.SandboxConfig
	if name := protoWP.GetSpec().GetSandboxConfigName(); name != "" {
		protoSC, err := st.GetSandboxConfig(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("while getting SandboxConfig %q: %w", name, err)
		}
		sc = protoSC
		if sc.GetSpec().GetSandboxClass() != class {
			return nil, fmt.Errorf("SandboxConfig %q has class %q but WorkerPool %s/%s is class %q",
				name, sc.GetSpec().GetSandboxClass(), poolNamespace, poolName, class)
		}
	} else {
		sc, err = defaultSandboxConfig(ctx, st, class)
		if err != nil {
			return nil, err
		}
	}

	return sandboxAssetsProto(class, sc), nil
}

// defaultSandboxConfig returns the single SandboxConfig marked Default for the
// given class, erroring if there are zero or more than one.
func defaultSandboxConfig(ctx context.Context, st store.Interface, class string) (*ateapipb.SandboxConfig, error) {
	all, err := st.ListSandboxConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing SandboxConfigs: %w", err)
	}
	var match *ateapipb.SandboxConfig
	for _, sc := range all {
		if sc.GetSpec().GetSandboxClass() == class && sc.GetSpec().GetDefault() {
			if match != nil {
				return nil, fmt.Errorf("multiple default SandboxConfigs for class %q (%q and %q)", class, match.GetName(), sc.GetName())
			}
			match = sc
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no default SandboxConfig for class %q; set one with spec.default=true or name one via WorkerPool.spec.sandboxConfigName", class)
	}
	return match, nil
}

// sandboxAssetsProto converts a resolved SandboxConfig into the proto atelet
// consumes.
func sandboxAssetsProto(class string, sc *ateapipb.SandboxConfig) *ateletpb.SandboxAssets {
	out := &ateletpb.SandboxAssets{
		SandboxClass: class,
		Assets:       make(map[string]*ateletpb.ArchAssets, len(sc.GetSpec().GetAssets())),
	}
	for arch, files := range sc.GetSpec().GetAssets() {
		archAssets := &ateletpb.ArchAssets{Files: make(map[string]*ateletpb.AssetFile, len(files.GetFiles()))}
		for name, f := range files.GetFiles() {
			archAssets.Files[name] = &ateletpb.AssetFile{Url: f.GetUrl(), Sha256: f.GetSha256()}
		}
		out.Assets[arch] = archAssets
	}
	return out
}
