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
	"errors"
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *Service) CreateActorTemplate(ctx context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.CreateActorTemplateResponse, error) {
	at := req.GetActorTemplate()
	if err := validateActorTemplate(at); err != nil {
		return nil, err
	}
	if err := s.requireAtespace(ctx, at.GetAtespace()); err != nil {
		return nil, err
	}
	if at.Status == nil {
		at.Status = &ateapipb.ActorTemplateStatus{}
	}
	if at.Status.Phase == "" {
		at.Status.Phase = string(atev1alpha1.PhaseInitial)
	}
	if err := s.persistence.CreateActorTemplate(ctx, at); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "ActorTemplate %s/%s already exists", at.GetAtespace(), at.GetName())
		}
		return nil, fmt.Errorf("while recording ActorTemplate: %w", err)
	}
	stored, err := s.persistence.GetActorTemplate(ctx, at.GetAtespace(), at.GetName())
	if err != nil {
		return nil, fmt.Errorf("while fetching recorded ActorTemplate: %w", err)
	}
	return &ateapipb.CreateActorTemplateResponse{ActorTemplate: stored}, nil
}

func (s *Service) GetActorTemplate(ctx context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.GetActorTemplateResponse, error) {
	ref := req.GetActorTemplate()
	if err := validateActorTemplateRef(ref); err != nil {
		return nil, err
	}
	at, err := s.persistence.GetActorTemplate(ctx, ref.GetAtespace(), ref.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "ActorTemplate %s/%s not found", ref.GetAtespace(), ref.GetName())
		}
		return nil, fmt.Errorf("while getting ActorTemplate: %w", err)
	}
	return &ateapipb.GetActorTemplateResponse{ActorTemplate: at}, nil
}

func (s *Service) ListActorTemplates(ctx context.Context, req *ateapipb.ListActorTemplatesRequest) (*ateapipb.ListActorTemplatesResponse, error) {
	if req.GetAtespace() != "" {
		if err := resources.ValidateAtespace(req.GetAtespace()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	ats, err := s.persistence.ListActorTemplates(ctx, req.GetAtespace())
	if err != nil {
		return nil, fmt.Errorf("while listing ActorTemplates: %w", err)
	}
	return &ateapipb.ListActorTemplatesResponse{ActorTemplates: ats}, nil
}

func (s *Service) UpdateActorTemplate(ctx context.Context, req *ateapipb.UpdateActorTemplateRequest) (*ateapipb.UpdateActorTemplateResponse, error) {
	at := req.GetActorTemplate()
	if err := validateActorTemplate(at); err != nil {
		return nil, err
	}
	if at.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "actor_template.version is required")
	}
	if err := s.persistence.UpdateActorTemplate(ctx, at, at.GetVersion()); err != nil {
		return nil, resourceStoreErr("ActorTemplate", fmt.Sprintf("%s/%s", at.GetAtespace(), at.GetName()), err)
	}
	return &ateapipb.UpdateActorTemplateResponse{ActorTemplate: at}, nil
}

func (s *Service) DeleteActorTemplate(ctx context.Context, req *ateapipb.DeleteActorTemplateRequest) (*ateapipb.DeleteActorTemplateResponse, error) {
	ref := req.GetActorTemplate()
	if err := validateActorTemplateRef(ref); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteActorTemplate(ctx, ref.GetAtespace(), ref.GetName()); err != nil {
		return nil, resourceStoreErr("ActorTemplate", fmt.Sprintf("%s/%s", ref.GetAtespace(), ref.GetName()), err)
	}
	return &ateapipb.DeleteActorTemplateResponse{}, nil
}

func (s *Service) CreateWorkerPool(ctx context.Context, req *ateapipb.CreateWorkerPoolRequest) (*ateapipb.CreateWorkerPoolResponse, error) {
	wp := req.GetWorkerPool()
	if err := validateWorkerPool(wp); err != nil {
		return nil, err
	}
	if err := s.persistence.CreateWorkerPool(ctx, wp); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "WorkerPool %s/%s already exists", wp.GetNamespace(), wp.GetName())
		}
		return nil, fmt.Errorf("while recording WorkerPool: %w", err)
	}
	stored, err := s.persistence.GetWorkerPool(ctx, wp.GetNamespace(), wp.GetName())
	if err != nil {
		return nil, fmt.Errorf("while fetching recorded WorkerPool: %w", err)
	}
	return &ateapipb.CreateWorkerPoolResponse{WorkerPool: stored}, nil
}

func (s *Service) GetWorkerPool(ctx context.Context, req *ateapipb.GetWorkerPoolRequest) (*ateapipb.GetWorkerPoolResponse, error) {
	ref := req.GetWorkerPool()
	if err := validateWorkerPoolRef(ref); err != nil {
		return nil, err
	}
	wp, err := s.persistence.GetWorkerPool(ctx, ref.GetNamespace(), ref.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "WorkerPool %s/%s not found", ref.GetNamespace(), ref.GetName())
		}
		return nil, fmt.Errorf("while getting WorkerPool: %w", err)
	}
	return &ateapipb.GetWorkerPoolResponse{WorkerPool: wp}, nil
}

func (s *Service) ListWorkerPools(ctx context.Context, req *ateapipb.ListWorkerPoolsRequest) (*ateapipb.ListWorkerPoolsResponse, error) {
	pools, err := s.persistence.ListWorkerPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing WorkerPools: %w", err)
	}
	return &ateapipb.ListWorkerPoolsResponse{WorkerPools: pools}, nil
}

func (s *Service) UpdateWorkerPool(ctx context.Context, req *ateapipb.UpdateWorkerPoolRequest) (*ateapipb.UpdateWorkerPoolResponse, error) {
	wp := req.GetWorkerPool()
	if err := validateWorkerPool(wp); err != nil {
		return nil, err
	}
	if wp.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "worker_pool.version is required")
	}
	if err := s.persistence.UpdateWorkerPool(ctx, wp, wp.GetVersion()); err != nil {
		return nil, resourceStoreErr("WorkerPool", fmt.Sprintf("%s/%s", wp.GetNamespace(), wp.GetName()), err)
	}
	return &ateapipb.UpdateWorkerPoolResponse{WorkerPool: wp}, nil
}

func (s *Service) DeleteWorkerPool(ctx context.Context, req *ateapipb.DeleteWorkerPoolRequest) (*ateapipb.DeleteWorkerPoolResponse, error) {
	ref := req.GetWorkerPool()
	if err := validateWorkerPoolRef(ref); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteWorkerPool(ctx, ref.GetNamespace(), ref.GetName()); err != nil {
		return nil, resourceStoreErr("WorkerPool", fmt.Sprintf("%s/%s", ref.GetNamespace(), ref.GetName()), err)
	}
	return &ateapipb.DeleteWorkerPoolResponse{}, nil
}

func (s *Service) CreateSandboxConfig(ctx context.Context, req *ateapipb.CreateSandboxConfigRequest) (*ateapipb.CreateSandboxConfigResponse, error) {
	sc := req.GetSandboxConfig()
	if err := validateSandboxConfig(sc); err != nil {
		return nil, err
	}
	if err := s.checkSandboxDefault(ctx, sc); err != nil {
		return nil, err
	}
	if err := s.persistence.CreateSandboxConfig(ctx, sc); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "SandboxConfig %s already exists", sc.GetName())
		}
		return nil, fmt.Errorf("while recording SandboxConfig: %w", err)
	}
	stored, err := s.persistence.GetSandboxConfig(ctx, sc.GetName())
	if err != nil {
		return nil, fmt.Errorf("while fetching recorded SandboxConfig: %w", err)
	}
	return &ateapipb.CreateSandboxConfigResponse{SandboxConfig: stored}, nil
}

func (s *Service) GetSandboxConfig(ctx context.Context, req *ateapipb.GetSandboxConfigRequest) (*ateapipb.GetSandboxConfigResponse, error) {
	if err := validateResourceName("name", req.GetName()); err != nil {
		return nil, err
	}
	sc, err := s.persistence.GetSandboxConfig(ctx, req.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "SandboxConfig %s not found", req.GetName())
		}
		return nil, fmt.Errorf("while getting SandboxConfig: %w", err)
	}
	return &ateapipb.GetSandboxConfigResponse{SandboxConfig: sc}, nil
}

func (s *Service) ListSandboxConfigs(ctx context.Context, req *ateapipb.ListSandboxConfigsRequest) (*ateapipb.ListSandboxConfigsResponse, error) {
	configs, err := s.persistence.ListSandboxConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing SandboxConfigs: %w", err)
	}
	return &ateapipb.ListSandboxConfigsResponse{SandboxConfigs: configs}, nil
}

func (s *Service) UpdateSandboxConfig(ctx context.Context, req *ateapipb.UpdateSandboxConfigRequest) (*ateapipb.UpdateSandboxConfigResponse, error) {
	sc := req.GetSandboxConfig()
	if err := validateSandboxConfig(sc); err != nil {
		return nil, err
	}
	if sc.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "sandbox_config.version is required")
	}
	if err := s.checkSandboxDefault(ctx, sc); err != nil {
		return nil, err
	}
	if err := s.persistence.UpdateSandboxConfig(ctx, sc, sc.GetVersion()); err != nil {
		return nil, resourceStoreErr("SandboxConfig", sc.GetName(), err)
	}
	return &ateapipb.UpdateSandboxConfigResponse{SandboxConfig: sc}, nil
}

func (s *Service) DeleteSandboxConfig(ctx context.Context, req *ateapipb.DeleteSandboxConfigRequest) (*ateapipb.DeleteSandboxConfigResponse, error) {
	if err := validateResourceName("name", req.GetName()); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteSandboxConfig(ctx, req.GetName()); err != nil {
		return nil, resourceStoreErr("SandboxConfig", req.GetName(), err)
	}
	return &ateapipb.DeleteSandboxConfigResponse{}, nil
}

func (s *Service) requireAtespace(ctx context.Context, name string) error {
	exists, err := s.persistence.AtespaceExists(ctx, name)
	if err != nil {
		return fmt.Errorf("while checking atespace: %w", err)
	}
	if !exists {
		return status.Errorf(codes.FailedPrecondition, "Atespace %s not found", name)
	}
	return nil
}

func (s *Service) checkSandboxDefault(ctx context.Context, sc *ateapipb.SandboxConfig) error {
	if !sc.GetSpec().GetDefault() {
		return nil
	}
	configs, err := s.persistence.ListSandboxConfigs(ctx)
	if err != nil {
		return fmt.Errorf("while checking default SandboxConfigs: %w", err)
	}
	for _, existing := range configs {
		if existing.GetName() != sc.GetName() && existing.GetSpec().GetSandboxClass() == sc.GetSpec().GetSandboxClass() && existing.GetSpec().GetDefault() {
			return status.Errorf(codes.FailedPrecondition, "default SandboxConfig for class %q already exists: %s", sc.GetSpec().GetSandboxClass(), existing.GetName())
		}
	}
	return nil
}

func resourceStoreErr(kind, name string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return status.Errorf(codes.NotFound, "%s %s not found", kind, name)
	}
	if errors.Is(err, store.ErrPersistenceRetry) {
		return status.Error(codes.Aborted, "concurrent update conflict, please retry")
	}
	return fmt.Errorf("while updating %s %s: %w", kind, name, err)
}

func validateActorTemplate(at *ateapipb.ActorTemplate) error {
	if at == nil {
		return status.Error(codes.InvalidArgument, "actor_template is required")
	}
	if err := validateActorTemplateRef(&ateapipb.ActorTemplateRef{Atespace: at.GetAtespace(), Name: at.GetName()}); err != nil {
		return err
	}
	if at.GetSpec().GetPauseImage() == "" {
		return status.Error(codes.InvalidArgument, "actor_template.spec.pause_image is required")
	}
	if at.GetSpec().GetSnapshotsConfig().GetLocation() == "" {
		return status.Error(codes.InvalidArgument, "actor_template.spec.snapshots_config.location is required")
	}
	return nil
}

func validateActorTemplateRef(ref *ateapipb.ActorTemplateRef) error {
	if ref == nil {
		return status.Error(codes.InvalidArgument, "actor_template is required")
	}
	if ref.GetAtespace() == "" {
		return status.Error(codes.InvalidArgument, "actor_template.atespace is required")
	}
	if err := resources.ValidateAtespace(ref.GetAtespace()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return validateResourceName("actor_template.name", ref.GetName())
}

func validateWorkerPool(wp *ateapipb.WorkerPool) error {
	if wp == nil {
		return status.Error(codes.InvalidArgument, "worker_pool is required")
	}
	if err := validateWorkerPoolRef(&ateapipb.WorkerPoolRef{Namespace: wp.GetNamespace(), Name: wp.GetName()}); err != nil {
		return err
	}
	if wp.GetSpec().GetAteomImage() == "" {
		return status.Error(codes.InvalidArgument, "worker_pool.spec.ateom_image is required")
	}
	if err := validateLabels(wp.GetLabels()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func validateWorkerPoolRef(ref *ateapipb.WorkerPoolRef) error {
	if ref == nil {
		return status.Error(codes.InvalidArgument, "worker_pool is required")
	}
	if ref.GetNamespace() == "" {
		return status.Error(codes.InvalidArgument, "worker_pool.namespace is required")
	}
	if errs := validation.IsDNS1123Label(ref.GetNamespace()); len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "invalid worker_pool.namespace %q: %s", ref.GetNamespace(), strings.Join(errs, "; "))
	}
	return validateResourceName("worker_pool.name", ref.GetName())
}

func validateSandboxConfig(sc *ateapipb.SandboxConfig) error {
	if sc == nil {
		return status.Error(codes.InvalidArgument, "sandbox_config is required")
	}
	if err := validateResourceName("sandbox_config.name", sc.GetName()); err != nil {
		return err
	}
	if sc.GetSpec().GetSandboxClass() == "" {
		return status.Error(codes.InvalidArgument, "sandbox_config.spec.sandbox_class is required")
	}
	return nil
}

func validateResourceName(field, name string) error {
	if name == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "invalid %s %q: %s", field, name, strings.Join(errs, "; "))
	}
	return nil
}

func validateLabels(labels map[string]string) error {
	for k, v := range labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return fmt.Errorf("invalid label key %q: %s", k, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("invalid label value %q for key %q: %s", v, k, strings.Join(errs, "; "))
		}
	}
	return nil
}
