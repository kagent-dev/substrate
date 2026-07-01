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

package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func GrantAtespaceToTemplateWorkerPools(t testing.TB, ctx context.Context, clients *Clients, atespace string, at *v1alpha1.ActorTemplate) {
	t.Helper()

	selector := labels.Everything()
	if at.Spec.WorkerSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(at.Spec.WorkerSelector)
		if err != nil {
			t.Fatalf("invalid WorkerSelector on ActorTemplate %s/%s: %v", at.Namespace, at.Name, err)
		}
		selector = sel
	}

	pools, err := clients.SubstrateAPI.ListWorkerPools(ctx, &ateapipb.ListWorkerPoolsRequest{})
	if err != nil {
		t.Fatalf("list WorkerPools: %v", err)
	}

	granted := 0
	for _, pool := range pools.GetWorkerPools() {
		if pool.GetSpec().GetSandboxClass() != string(at.Spec.SandboxClass) || !selector.Matches(labels.Set(pool.GetLabels())) {
			continue
		}
		_, err := clients.SubstrateAPI.CreateWorkerPoolGrant(ctx, &ateapipb.CreateWorkerPoolGrantRequest{
			WorkerPoolGrant: &ateapipb.WorkerPoolGrant{
				Atespace:   atespace,
				Name:       pool.GetName(),
				WorkerPool: &ateapipb.WorkerPoolRef{Name: pool.GetName()},
			},
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			t.Fatalf("grant %s access to WorkerPool %s: %v", atespace, pool.GetName(), err)
		}
		granted++
	}
	if granted == 0 {
		t.Fatalf("no WorkerPool matched ActorTemplate %s/%s for atespace %s: %s", at.Namespace, at.Name, atespace, describeTemplateWorkerPoolMatch(at))
	}
}

func describeTemplateWorkerPoolMatch(at *v1alpha1.ActorTemplate) string {
	if at.Spec.WorkerSelector == nil {
		return fmt.Sprintf("sandboxClass=%q selector=<none>", at.Spec.SandboxClass)
	}
	return fmt.Sprintf("sandboxClass=%q selector=%v", at.Spec.SandboxClass, at.Spec.WorkerSelector.MatchLabels)
}
