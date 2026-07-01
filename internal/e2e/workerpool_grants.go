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

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

func GrantAtespaceToTemplateWorkerPools(t testing.TB, ctx context.Context, clients *Clients, atespace string, at *ateapipb.ActorTemplate) {
	t.Helper()

	selector, err := labelSelector(at.GetSpec().GetWorkerSelector())
	if err != nil {
		t.Fatalf("invalid WorkerSelector on ActorTemplate %s/%s: %v", at.GetAtespace(), at.GetName(), err)
	}

	pools, err := clients.SubstrateAPI.ListWorkerPools(ctx, &ateapipb.ListWorkerPoolsRequest{})
	if err != nil {
		t.Fatalf("list WorkerPools: %v", err)
	}

	granted := 0
	for _, pool := range pools.GetWorkerPools() {
		if pool.GetSpec().GetSandboxClass() != at.GetSpec().GetSandboxClass() || !selector.Matches(labels.Set(pool.GetLabels())) {
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
		t.Fatalf("no WorkerPool matched ActorTemplate %s/%s for atespace %s: %s", at.GetAtespace(), at.GetName(), atespace, describeTemplateWorkerPoolMatch(at))
	}
}

func describeTemplateWorkerPoolMatch(at *ateapipb.ActorTemplate) string {
	if at.GetSpec().GetWorkerSelector() == nil {
		return fmt.Sprintf("sandboxClass=%q selector=<none>", at.GetSpec().GetSandboxClass())
	}
	return fmt.Sprintf("sandboxClass=%q selector=%v", at.GetSpec().GetSandboxClass(), at.GetSpec().GetWorkerSelector().GetMatchLabels())
}

func labelSelector(in *ateapipb.LabelSelector) (labels.Selector, error) {
	if in == nil {
		return labels.Everything(), nil
	}
	selector := labels.SelectorFromSet(labels.Set(in.GetMatchLabels()))
	for _, expr := range in.GetMatchExpressions() {
		op, err := selectionOperator(expr.GetOperator())
		if err != nil {
			return nil, err
		}
		req, err := labels.NewRequirement(expr.GetKey(), op, expr.GetValues())
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*req)
	}
	return selector, nil
}

func selectionOperator(op string) (selection.Operator, error) {
	switch op {
	case "In":
		return selection.In, nil
	case "NotIn":
		return selection.NotIn, nil
	case "Exists":
		return selection.Exists, nil
	case "DoesNotExist":
		return selection.DoesNotExist, nil
	default:
		return "", fmt.Errorf("unsupported selector operator %q", op)
	}
}
