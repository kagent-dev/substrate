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

package router

import (
	"context"
	"fmt"

	v1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// atStore defines an interface for retrieving a collection of ActorTemplates.
type atStore interface {
	readyTemplates(ctx context.Context) ([]*v1alpha1.ActorTemplate, error)
}

type apiATStore struct {
	apiClient ateapipb.ControlClient
}

func newAPIATStore(apiClient ateapipb.ControlClient) *apiATStore {
	return &apiATStore{apiClient: apiClient}
}

func (t *apiATStore) readyTemplates(ctx context.Context) ([]*v1alpha1.ActorTemplate, error) {
	resp, err := t.apiClient.ListActorTemplates(ctx, &ateapipb.ListActorTemplatesRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list ActorTemplates: %w", err)
	}

	var templates []*v1alpha1.ActorTemplate
	for _, at := range resp.GetActorTemplates() {
		if at.GetStatus().GetPhase() != string(v1alpha1.PhaseReady) {
			continue
		}
		templates = append(templates, &v1alpha1.ActorTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      at.GetName(),
				Namespace: at.GetAtespace(),
			},
			Status: v1alpha1.ActorTemplateStatus{
				Phase:          v1alpha1.PhaseType(at.GetStatus().GetPhase()),
				GoldenSnapshot: at.GetStatus().GetGoldenSnapshot(),
				GoldenActorID:  at.GetStatus().GetGoldenActorId(),
			},
		})
	}
	return templates, nil
}
