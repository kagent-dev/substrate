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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type APIWorkerPoolProjector struct {
	client.Client
	AteClient ateapipb.ControlClient
}

func (p *APIWorkerPoolProjector) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("api-workerpool-projector")
	for {
		stream, err := p.AteClient.WatchWorkerPools(ctx, &ateapipb.WatchWorkerPoolsRequest{})
		if err != nil {
			log.Error(err, "watch WorkerPools failed")
			if !sleepOrDone(ctx, time.Second) {
				return nil
			}
			continue
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				log.Error(err, "WorkerPool watch stream closed")
				break
			}
			if err := p.applyEvent(ctx, event); err != nil {
				log.Error(err, "project WorkerPool", "name", event.GetWorkerPool().GetName())
			}
		}
		if !sleepOrDone(ctx, time.Second) {
			return nil
		}
	}
}

func (p *APIWorkerPoolProjector) applyEvent(ctx context.Context, event *ateapipb.WatchWorkerPoolsResponse) error {
	wp := event.GetWorkerPool()
	if wp.GetName() == "" || wp.GetSpec().GetDeploymentAtespace() == "" {
		return nil
	}
	if event.GetType() == ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_DELETED {
		return client.IgnoreNotFound(p.Delete(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: wp.GetSpec().GetDeploymentAtespace(),
				Name:      deploymentName(wp.GetName()),
			},
		}))
	}
	depAC, err := buildDeploymentApplyConfig(wp)
	if err != nil {
		return err
	}
	if err := p.Apply(ctx, depAC, client.FieldOwner(workerPoolFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply Deployment: %w", err)
	}
	return nil
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
