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

	"github.com/agent-substrate/substrate/internal/resources"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	GoldenSnapshotCreationReason = "GoldenSnapshotCreation"

	// goldenSnapshotWarmup is the default wall-clock delay between resuming
	// the golden actor and taking its snapshot, used as a coarse "give the
	// workload time to finish initializing" fallback for templates without
	// a readiness probe. Templates whose containers all declare readyz skip
	// this wait because ResumeActor already blocks until readyz reports 200.
	goldenSnapshotWarmup = 20 * time.Second
)

type APIActorTemplateProjector struct {
	AteClient ateapipb.ControlClient
}

func (p *APIActorTemplateProjector) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("api-actortemplate-projector")
	for {
		stream, err := p.AteClient.WatchActorTemplates(ctx, &ateapipb.WatchActorTemplatesRequest{})
		if err != nil {
			log.Error(err, "watch ActorTemplates failed")
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
				log.Error(err, "ActorTemplate watch stream closed")
				break
			}
			if event.GetType() == ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_DELETED {
				continue
			}
			if err := p.reconcile(ctx, event.GetActorTemplate()); err != nil {
				log.Error(err, "reconcile ActorTemplate", "atespace", event.GetActorTemplate().GetAtespace(), "name", event.GetActorTemplate().GetName())
			}
		}
		if !sleepOrDone(ctx, time.Second) {
			return nil
		}
	}
}

func (p *APIActorTemplateProjector) reconcile(ctx context.Context, at *ateapipb.ActorTemplate) error {
	switch at.GetStatus().GetPhase() {
	case string(atev1alpha1.PhaseInitial):
		return p.reconcileInitial(ctx, at)
	case string(atev1alpha1.PhaseResumeGoldenActor):
		return p.reconcileResumeGolden(ctx, at)
	case string(atev1alpha1.PhaseWaitGoldenActor):
		return p.reconcileWaitGolden(ctx, at)
	case string(atev1alpha1.PhaseReady):
		return nil
	default:
		return fmt.Errorf("unrecognized phase %q", at.GetStatus().GetPhase())
	}
}

func (p *APIActorTemplateProjector) reconcileInitial(ctx context.Context, at *ateapipb.ActorTemplate) error {
	actorID := uuid.NewString()
	_, err := p.AteClient.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Name: resources.GoldenActorAtespace,
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("while ensuring atespace %q: %w", resources.GoldenActorAtespace, err)
	}
	if err := p.grantGoldenActorAccess(ctx, at); err != nil {
		return err
	}
	_, err = p.AteClient.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorRef:      &ateapipb.ActorRef{Atespace: resources.GoldenActorAtespace, Name: actorID},
		ActorTemplate: &ateapipb.ActorTemplateRef{Atespace: at.GetAtespace(), Name: at.GetName()},
	})
	if err != nil {
		return fmt.Errorf("while creating golden actor: %w", err)
	}
	next := proto.Clone(at).(*ateapipb.ActorTemplate)
	ensureActorTemplateStatus(next)
	next.Status.Phase = string(atev1alpha1.PhaseResumeGoldenActor)
	next.Status.GoldenActorId = actorID
	_, err = p.AteClient.UpdateActorTemplate(ctx, actorTemplateStatusUpdate(next))
	return err
}

func (p *APIActorTemplateProjector) reconcileResumeGolden(ctx context.Context, at *ateapipb.ActorTemplate) error {
	_, err := p.AteClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
		ActorRef: &ateapipb.ActorRef{Atespace: resources.GoldenActorAtespace, Name: at.GetStatus().GetGoldenActorId()},
	})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			go func() {
				if sleepOrDone(ctx, time.Second) {
					_ = p.reconcile(ctx, at)
				}
			}()
			return nil
		}
		return fmt.Errorf("while resuming golden actor: %w", err)
	}
	next := proto.Clone(at).(*ateapipb.ActorTemplate)
	ensureActorTemplateStatus(next)
	next.Status.Phase = string(atev1alpha1.PhaseWaitGoldenActor)
	next.Status.TakeGoldenSnapshotAt = timestamppb.New(time.Now().Add(goldenSnapshotWarmupForProto(at)))
	_, err = p.AteClient.UpdateActorTemplate(ctx, actorTemplateStatusUpdate(next))
	return err
}

func (p *APIActorTemplateProjector) reconcileWaitGolden(ctx context.Context, at *ateapipb.ActorTemplate) error {
	if at.GetStatus().GetTakeGoldenSnapshotAt() != nil {
		if rem := time.Until(at.GetStatus().GetTakeGoldenSnapshotAt().AsTime()); rem > 0 {
			go func() {
				if sleepOrDone(ctx, rem) {
					_ = p.reconcile(ctx, at)
				}
			}()
			return nil
		}
	}
	resp, err := p.AteClient.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		ActorRef: &ateapipb.ActorRef{Atespace: resources.GoldenActorAtespace, Name: at.GetStatus().GetGoldenActorId()},
	})
	if err != nil {
		return fmt.Errorf("while suspending golden actor: %w", err)
	}
	if resp.GetActor().GetLatestSnapshotInfo().GetType() != ateapipb.SnapshotType_SNAPSHOT_TYPE_EXTERNAL {
		return fmt.Errorf("unexpected snapshot type for golden actor: %v", resp.GetActor().GetLatestSnapshotInfo().GetType())
	}
	next := proto.Clone(at).(*ateapipb.ActorTemplate)
	ensureActorTemplateStatus(next)
	next.Status.GoldenSnapshot = resp.GetActor().GetLatestSnapshotInfo().GetExternal().GetSnapshotUriPrefix()
	next.Status.Phase = string(atev1alpha1.PhaseReady)
	next.Status.Conditions = []*ateapipb.Condition{{
		Type:               "Ready",
		Status:             string(metav1.ConditionTrue),
		LastTransitionTime: timestamppb.Now(),
		Reason:             "Ready",
		Message:            "Actor template is ready for use",
	}}
	_, err = p.AteClient.UpdateActorTemplate(ctx, actorTemplateStatusUpdate(next))
	return err
}

func actorTemplateStatusUpdate(at *ateapipb.ActorTemplate) *ateapipb.UpdateActorTemplateRequest {
	return &ateapipb.UpdateActorTemplateRequest{
		ActorTemplate: at,
		UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"status"}},
	}
}

func ensureActorTemplateStatus(at *ateapipb.ActorTemplate) {
	if at.Status == nil {
		at.Status = &ateapipb.ActorTemplateStatus{}
	}
}

func (p *APIActorTemplateProjector) grantGoldenActorAccess(ctx context.Context, at *ateapipb.ActorTemplate) error {
	selector := labels.Everything()
	if at.GetSpec().GetWorkerSelector() != nil {
		sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels:      copyStringMap(at.GetSpec().GetWorkerSelector().GetMatchLabels()),
			MatchExpressions: protoLabelSelectorRequirementsToAPI(at.GetSpec().GetWorkerSelector().GetMatchExpressions()),
		})
		if err != nil {
			return fmt.Errorf("invalid WorkerSelector: %w", err)
		}
		selector = sel
	}
	pools, err := p.AteClient.ListWorkerPools(ctx, &ateapipb.ListWorkerPoolsRequest{})
	if err != nil {
		return fmt.Errorf("while listing WorkerPools for golden actor grant: %w", err)
	}
	for _, pool := range pools.GetWorkerPools() {
		if pool.GetSpec().GetSandboxClass() != at.GetSpec().GetSandboxClass() || !selector.Matches(labels.Set(pool.GetLabels())) {
			continue
		}
		_, err := p.AteClient.CreateWorkerPoolGrant(ctx, &ateapipb.CreateWorkerPoolGrantRequest{
			WorkerPoolGrant: &ateapipb.WorkerPoolGrant{
				Atespace:   resources.GoldenActorAtespace,
				Name:       pool.GetName(),
				WorkerPool: &ateapipb.WorkerPoolRef{Name: pool.GetName()},
			},
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("while granting golden actor access to WorkerPool %s: %w", pool.GetName(), err)
		}
	}
	return nil
}

func protoLabelSelectorRequirementsToAPI(in []*ateapipb.LabelSelectorRequirement) []metav1.LabelSelectorRequirement {
	out := make([]metav1.LabelSelectorRequirement, 0, len(in))
	for _, req := range in {
		out = append(out, metav1.LabelSelectorRequirement{
			Key:      req.GetKey(),
			Operator: metav1.LabelSelectorOperator(req.GetOperator()),
			Values:   append([]string(nil), req.GetValues()...),
		})
	}
	return out
}

func goldenSnapshotWarmupForProto(at *ateapipb.ActorTemplate) time.Duration {
	containers := at.GetSpec().GetContainers()
	if len(containers) == 0 {
		return goldenSnapshotWarmup
	}
	for _, c := range containers {
		if c.GetReadyz() == nil {
			return goldenSnapshotWarmup
		}
	}
	return 0
}
