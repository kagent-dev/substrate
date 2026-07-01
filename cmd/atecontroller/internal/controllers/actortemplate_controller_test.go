// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestGoldenSnapshotWarmupFor(t *testing.T) {
	probe := &ateapipb.ContainerReadyz{
		HttpGet: &ateapipb.HTTPGetAction{Port: 80},
	}

	tests := []struct {
		name       string
		containers []*ateapipb.Container
		wantZero   bool
	}{
		{
			name:       "no containers keeps default warmup",
			containers: nil,
			wantZero:   false,
		},
		{
			name: "all containers have readyz skips warmup",
			containers: []*ateapipb.Container{
				{Name: "a", Readyz: probe},
				{Name: "b", Readyz: probe},
			},
			wantZero: true,
		},
		{
			name: "single container with readyz skips warmup",
			containers: []*ateapipb.Container{
				{Name: "a", Readyz: probe},
			},
			wantZero: true,
		},
		{
			name: "mixed containers keep warmup",
			containers: []*ateapipb.Container{
				{Name: "a", Readyz: probe},
				{Name: "b"},
			},
			wantZero: false,
		},
		{
			name: "no readyz anywhere keeps warmup",
			containers: []*ateapipb.Container{
				{Name: "a"},
				{Name: "b"},
			},
			wantZero: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := &ateapipb.ActorTemplate{
				Spec: &ateapipb.ActorTemplateSpec{Containers: tt.containers},
			}
			got := goldenSnapshotWarmupForProto(at)
			if tt.wantZero && got != 0 {
				t.Errorf("goldenSnapshotWarmupFor = %v, want 0", got)
			}
			if !tt.wantZero && got != goldenSnapshotWarmup {
				t.Errorf("goldenSnapshotWarmupFor = %v, want %v", got, goldenSnapshotWarmup)
			}
		})
	}
}
