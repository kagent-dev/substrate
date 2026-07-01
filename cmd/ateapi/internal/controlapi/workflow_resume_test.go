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
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/types"
)

func pool(namespace, name string, labels map[string]string) *ateapipb.WorkerPool {
	return &ateapipb.WorkerPool{
		Name:   name,
		Labels: labels,
		Spec: &ateapipb.WorkerPoolSpec{
			DeploymentAtespace: namespace,
		},
	}
}

func poolWithClass(namespace, name string, class string, labels map[string]string) *ateapipb.WorkerPool {
	p := pool(namespace, name, labels)
	p.Spec.SandboxClass = class
	return p
}

func TestEligibleWorkerPools(t *testing.T) {
	tests := []struct {
		name              string
		pools             []*ateapipb.WorkerPool
		templateClass     string
		templateSelector  *ateapipb.LabelSelector
		actorSelector     *ateapipb.Selector
		wantEligibleNames []string // pool names expected in the result
	}{
		{
			name: "both nil matches everything",
			pools: []*ateapipb.WorkerPool{
				pool("ns", "a", map[string]string{"foo": "bar"}),
				pool("ns", "b", nil),
			},
			templateSelector:  nil,
			actorSelector:     nil,
			wantEligibleNames: []string{"a", "b"},
		},
		{
			name: "template selector only",
			pools: []*ateapipb.WorkerPool{
				pool("ns", "match", map[string]string{"workload": "code-sandbox"}),
				pool("ns", "nomatch", map[string]string{"workload": "browser-agent"}),
			},
			templateSelector: &ateapipb.LabelSelector{
				MatchLabels: map[string]string{"workload": "code-sandbox"},
			},
			actorSelector:     nil,
			wantEligibleNames: []string{"match"},
		},
		{
			name: "actor selector only",
			pools: []*ateapipb.WorkerPool{
				pool("ns", "match", map[string]string{"tier": "paid"}),
				pool("ns", "nomatch", map[string]string{"tier": "free"}),
			},
			templateSelector: nil,
			actorSelector: &ateapipb.Selector{
				MatchLabels: map[string]string{"tier": "paid"},
			},
			wantEligibleNames: []string{"match"},
		},
		{
			name: "AND of two selectors on the same pool",
			pools: []*ateapipb.WorkerPool{
				pool("ns", "both", map[string]string{"workload": "code-sandbox", "tier": "paid"}),
				pool("ns", "template-only", map[string]string{"workload": "code-sandbox", "tier": "free"}),
				pool("ns", "actor-only", map[string]string{"workload": "browser-agent", "tier": "paid"}),
				pool("ns", "neither", map[string]string{"workload": "browser-agent", "tier": "free"}),
			},
			templateSelector: &ateapipb.LabelSelector{
				MatchLabels: map[string]string{"workload": "code-sandbox"},
			},
			actorSelector: &ateapipb.Selector{
				MatchLabels: map[string]string{"tier": "paid"},
			},
			wantEligibleNames: []string{"both"},
		},
		{
			name: "disjoint label keys: independent evaluation, not a merged map",
			pools: []*ateapipb.WorkerPool{
				// Has the template's key/value but not the actor's.
				pool("ns", "template-key-only", map[string]string{"workload": "x"}),
				// Has the actor's key/value but not the template's.
				pool("ns", "actor-key-only", map[string]string{"zone": "y"}),
				// Has both keys with matching values: must be the only eligible pool.
				pool("ns", "both-keys", map[string]string{"workload": "x", "zone": "y"}),
			},
			templateSelector: &ateapipb.LabelSelector{
				MatchLabels: map[string]string{"workload": "x"},
			},
			actorSelector: &ateapipb.Selector{
				MatchLabels: map[string]string{"zone": "y"},
			},
			wantEligibleNames: []string{"both-keys"},
		},
		{
			name: "no eligible pool",
			pools: []*ateapipb.WorkerPool{
				pool("ns", "a", map[string]string{"workload": "x"}),
			},
			templateSelector: &ateapipb.LabelSelector{
				MatchLabels: map[string]string{"workload": "y"},
			},
			actorSelector:     nil,
			wantEligibleNames: nil,
		},
		{
			name: "microvm template matches only microvm pools",
			pools: []*ateapipb.WorkerPool{
				poolWithClass("ns", "micro", sandboxClassMicroVM, nil),
				poolWithClass("ns", "gvisor", sandboxClassGvisor, nil),
			},
			templateClass:     sandboxClassMicroVM,
			wantEligibleNames: []string{"micro"},
		},
		{
			name: "gvisor template excludes microvm pools",
			pools: []*ateapipb.WorkerPool{
				poolWithClass("ns", "micro", sandboxClassMicroVM, nil),
				poolWithClass("ns", "gvisor", sandboxClassGvisor, nil),
			},
			templateClass:     sandboxClassGvisor,
			wantEligibleNames: []string{"gvisor"},
		},
		{
			name: "class gate AND's with label selector",
			pools: []*ateapipb.WorkerPool{
				poolWithClass("ns", "match", sandboxClassMicroVM, map[string]string{"tier": "paid"}),
				// Right class, wrong label.
				poolWithClass("ns", "wrong-label", sandboxClassMicroVM, map[string]string{"tier": "free"}),
				// Right label, wrong class.
				poolWithClass("ns", "wrong-class", sandboxClassGvisor, map[string]string{"tier": "paid"}),
			},
			templateClass: sandboxClassMicroVM,
			templateSelector: &ateapipb.LabelSelector{
				MatchLabels: map[string]string{"tier": "paid"},
			},
			wantEligibleNames: []string{"match"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eligibleWorkerPools(tt.pools, tt.templateClass, tt.templateSelector, tt.actorSelector)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			wantKeys := make(map[types.NamespacedName]struct{}, len(tt.wantEligibleNames))
			for _, name := range tt.wantEligibleNames {
				wantKeys[types.NamespacedName{Namespace: "ns", Name: name}] = struct{}{}
			}

			if len(got) != len(wantKeys) {
				t.Fatalf("got %d eligible pools, want %d: got=%v want=%v", len(got), len(wantKeys), got, wantKeys)
			}
			for k := range wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("expected pool %v to be eligible, but it was not: got=%v", k, got)
				}
			}
		})
	}
}
