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
	"strings"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

func (s *Service) PrepareActorTemplate(ctx context.Context, req *ateapipb.PrepareActorTemplateRequest) (*ateapipb.PrepareActorTemplateResponse, error) {
	if req.GetActorTemplateNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_template_namespace is required")
	}
	if req.GetActorTemplateName() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_template_name is required")
	}

	at, err := s.actorTemplateLister.ActorTemplates(req.GetActorTemplateNamespace()).Get(req.GetActorTemplateName())
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "ActorTemplate %s/%s not found", req.GetActorTemplateNamespace(), req.GetActorTemplateName())
		}
		return nil, fmt.Errorf("while getting ActorTemplate: %w", err)
	}
	wpRef := at.Spec.WorkerPoolRef
	wpNS := wpRef.Namespace
	if wpNS == "" {
		wpNS = at.Namespace
	}
	wp, err := s.workerPoolLister.WorkerPools(wpNS).Get(wpRef.Name)
	if err != nil {
		return nil, fmt.Errorf("while getting WorkerPool: %w", err)
	}
	if wp.Spec.Backend != atev1alpha1.AteomBackendCloudHypervisor {
		return nil, status.Errorf(codes.FailedPrecondition, "ActorTemplate %s/%s uses WorkerPool backend %q, not %q", at.Namespace, at.Name, wp.Spec.Backend, atev1alpha1.AteomBackendCloudHypervisor)
	}

	worker, err := s.pickFreeWorker(ctx, wp.Namespace, wp.Name)
	if err != nil {
		return nil, err
	}
	ateletConn, err := s.dialer.DialForWorker(worker.GetWorkerNamespace(), worker.GetWorkerPod())
	if err != nil {
		return nil, err
	}
	client := ateletpb.NewAteomHerderClient(ateletConn)

	goldenSnapshotURI, err := cloudHypervisorGoldenSnapshotURI(at)
	if err != nil {
		return nil, err
	}
	resp, err := client.PrepareTemplate(ctx, &ateletpb.PrepareTemplateRequest{
		TargetAteomUid:         worker.GetWorkerPodUid(),
		ActorTemplateNamespace: at.Namespace,
		ActorTemplateName:      at.Name,
		Spec:                   buildAteletWorkloadSpec(at),
		GoldenSnapshotUri:      goldenSnapshotURI,
	})
	if err != nil {
		return nil, fmt.Errorf("while preparing template on atelet: %w", err)
	}
	return &ateapipb.PrepareActorTemplateResponse{GoldenSnapshot: resp.GetGoldenSnapshotUri()}, nil
}

func (s *Service) pickFreeWorker(ctx context.Context, namespace, pool string) (*ateapipb.Worker, error) {
	workers, err := s.persistence.ListWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing workers: %w", err)
	}
	for _, worker := range workers {
		if worker.GetActorId() == "" && worker.GetWorkerNamespace() == namespace && worker.GetWorkerPool() == pool {
			return worker, nil
		}
	}
	return nil, status.Errorf(codes.FailedPrecondition, "no free workers available in WorkerPool %s/%s", namespace, pool)
}

func cloudHypervisorGoldenSnapshotURI(at *atev1alpha1.ActorTemplate) (string, error) {
	if len(at.Spec.Containers) == 0 {
		return "", status.Error(codes.InvalidArgument, "ActorTemplate has no containers")
	}
	base := strings.TrimSuffix(at.Spec.SnapshotsConfig.Location, "/")
	if base == "" {
		return "", status.Error(codes.InvalidArgument, "snapshotsConfig.location is required")
	}
	return fmt.Sprintf("%s/templates/%s/%s/cloud-hypervisor/golden/%s.tar.zstd", base, at.Namespace, at.Name, at.Spec.Containers[0].Name), nil
}

func buildAteletWorkloadSpec(at *atev1alpha1.ActorTemplate) *ateletpb.WorkloadSpec {
	workloadSpec := &ateletpb.WorkloadSpec{
		PauseImage: at.Spec.PauseImage,
	}
	for _, ctr := range at.Spec.Containers {
		ateletCtr := &ateletpb.Container{
			Name:    ctr.Name,
			Image:   ctr.Image,
			Command: ctr.Command,
		}
		for _, env := range ctr.Env {
			ateletCtr.Env = append(ateletCtr.Env, &ateletpb.EnvEntry{
				Name:  env.Name,
				Value: env.Value,
			})
		}
		workloadSpec.Containers = append(workloadSpec.Containers, ateletCtr)
	}
	return workloadSpec
}

func buildAteletRunscConfig(at *atev1alpha1.ActorTemplate) *ateletpb.RunscConfig {
	runscCfg := &ateletpb.RunscConfig{}
	if at.Spec.Runsc.AMD64 != nil {
		runscCfg.Amd64 = &ateletpb.RunscPlatformConfig{
			Sha256Hash: at.Spec.Runsc.AMD64.SHA256Hash,
			Url:        at.Spec.Runsc.AMD64.URL,
		}
	}
	if at.Spec.Runsc.ARM64 != nil {
		runscCfg.Arm64 = &ateletpb.RunscPlatformConfig{
			Sha256Hash: at.Spec.Runsc.ARM64.SHA256Hash,
			Url:        at.Spec.Runsc.ARM64.URL,
		}
	}
	if at.Spec.Runsc.Authentication.GCP != nil {
		runscCfg.Authentication = &ateletpb.AuthenticationConfig{Gcp: &ateletpb.GCPAuthenticationConfig{Use: true}}
	}
	return runscCfg
}
