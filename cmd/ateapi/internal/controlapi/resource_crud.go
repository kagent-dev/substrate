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
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *Service) CreateActorTemplate(ctx context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
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
	return stored, nil
}

func (s *Service) GetActorTemplate(ctx context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
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
	return at, nil
}

func (s *Service) ListActorTemplates(ctx context.Context, req *ateapipb.ListActorTemplatesRequest) (*ateapipb.ListActorTemplatesResponse, error) {
	if err := validatePageSize(req.GetPageSize()); err != nil {
		return nil, err
	}
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

func (s *Service) UpdateActorTemplate(ctx context.Context, req *ateapipb.UpdateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	if err := validateUpdateMask(req.GetUpdateMask()); err != nil {
		return nil, err
	}
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
	return at, nil
}

func (s *Service) DeleteActorTemplate(ctx context.Context, req *ateapipb.DeleteActorTemplateRequest) (*emptypb.Empty, error) {
	ref := req.GetActorTemplate()
	if err := validateActorTemplateRef(ref); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteActorTemplate(ctx, ref.GetAtespace(), ref.GetName()); err != nil {
		return nil, resourceStoreErr("ActorTemplate", fmt.Sprintf("%s/%s", ref.GetAtespace(), ref.GetName()), err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) CreateWorkerPool(ctx context.Context, req *ateapipb.CreateWorkerPoolRequest) (*ateapipb.WorkerPool, error) {
	wp := req.GetWorkerPool()
	if err := validateWorkerPool(wp); err != nil {
		return nil, err
	}
	if err := s.persistence.CreateWorkerPool(ctx, wp); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "WorkerPool %s already exists", wp.GetName())
		}
		return nil, fmt.Errorf("while recording WorkerPool: %w", err)
	}
	stored, err := s.persistence.GetWorkerPool(ctx, wp.GetName())
	if err != nil {
		return nil, fmt.Errorf("while fetching recorded WorkerPool: %w", err)
	}
	return stored, nil
}

func (s *Service) GetWorkerPool(ctx context.Context, req *ateapipb.GetWorkerPoolRequest) (*ateapipb.WorkerPool, error) {
	ref := req.GetWorkerPool()
	if err := validateWorkerPoolRef(ref); err != nil {
		return nil, err
	}
	wp, err := s.persistence.GetWorkerPool(ctx, ref.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "WorkerPool %s not found", ref.GetName())
		}
		return nil, fmt.Errorf("while getting WorkerPool: %w", err)
	}
	return wp, nil
}

func (s *Service) ListWorkerPools(ctx context.Context, req *ateapipb.ListWorkerPoolsRequest) (*ateapipb.ListWorkerPoolsResponse, error) {
	if err := validatePageSize(req.GetPageSize()); err != nil {
		return nil, err
	}
	pools, err := s.persistence.ListWorkerPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing WorkerPools: %w", err)
	}
	return &ateapipb.ListWorkerPoolsResponse{WorkerPools: pools}, nil
}

func (s *Service) UpdateWorkerPool(ctx context.Context, req *ateapipb.UpdateWorkerPoolRequest) (*ateapipb.WorkerPool, error) {
	if err := validateUpdateMask(req.GetUpdateMask()); err != nil {
		return nil, err
	}
	wp := req.GetWorkerPool()
	if err := validateWorkerPool(wp); err != nil {
		return nil, err
	}
	if wp.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "worker_pool.version is required")
	}
	if err := s.persistence.UpdateWorkerPool(ctx, wp, wp.GetVersion()); err != nil {
		return nil, resourceStoreErr("WorkerPool", wp.GetName(), err)
	}
	return wp, nil
}

func (s *Service) DeleteWorkerPool(ctx context.Context, req *ateapipb.DeleteWorkerPoolRequest) (*emptypb.Empty, error) {
	ref := req.GetWorkerPool()
	if err := validateWorkerPoolRef(ref); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteWorkerPool(ctx, ref.GetName()); err != nil {
		return nil, resourceStoreErr("WorkerPool", ref.GetName(), err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) CreateSandboxConfig(ctx context.Context, req *ateapipb.CreateSandboxConfigRequest) (*ateapipb.SandboxConfig, error) {
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
	return stored, nil
}

func (s *Service) GetSandboxConfig(ctx context.Context, req *ateapipb.GetSandboxConfigRequest) (*ateapipb.SandboxConfig, error) {
	ref := req.GetSandboxConfig()
	if ref == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox_config is required")
	}
	if err := validateResourceName("sandbox_config.name", ref.GetName()); err != nil {
		return nil, err
	}
	sc, err := s.persistence.GetSandboxConfig(ctx, ref.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "SandboxConfig %s not found", ref.GetName())
		}
		return nil, fmt.Errorf("while getting SandboxConfig: %w", err)
	}
	return sc, nil
}

func (s *Service) ListSandboxConfigs(ctx context.Context, req *ateapipb.ListSandboxConfigsRequest) (*ateapipb.ListSandboxConfigsResponse, error) {
	if err := validatePageSize(req.GetPageSize()); err != nil {
		return nil, err
	}
	configs, err := s.persistence.ListSandboxConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing SandboxConfigs: %w", err)
	}
	return &ateapipb.ListSandboxConfigsResponse{SandboxConfigs: configs}, nil
}

func (s *Service) UpdateSandboxConfig(ctx context.Context, req *ateapipb.UpdateSandboxConfigRequest) (*ateapipb.SandboxConfig, error) {
	if err := validateUpdateMask(req.GetUpdateMask()); err != nil {
		return nil, err
	}
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
	return sc, nil
}

func (s *Service) DeleteSandboxConfig(ctx context.Context, req *ateapipb.DeleteSandboxConfigRequest) (*emptypb.Empty, error) {
	ref := req.GetSandboxConfig()
	if ref == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox_config is required")
	}
	if err := validateResourceName("sandbox_config.name", ref.GetName()); err != nil {
		return nil, err
	}
	if err := s.persistence.DeleteSandboxConfig(ctx, ref.GetName()); err != nil {
		return nil, resourceStoreErr("SandboxConfig", ref.GetName(), err)
	}
	return &emptypb.Empty{}, nil
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
	if err := validateWorkerPoolRef(&ateapipb.WorkerPoolRef{Name: wp.GetName()}); err != nil {
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

func validateUpdateMask(mask *fieldmaskpb.FieldMask) error {
	// TODO: Implement true field-mask patch semantics once upstream has a
	// precedent to follow. This POC requires and validates update_mask for API
	// shape compliance, but Update* still replaces the submitted resource body.
	if mask == nil || len(mask.GetPaths()) == 0 {
		return status.Error(codes.InvalidArgument, "update_mask is required")
	}
	for _, path := range mask.GetPaths() {
		if path == "*" {
			return status.Error(codes.InvalidArgument, "update_mask '*' is not supported")
		}
	}
	return nil
}

func validatePageSize(pageSize int32) error {
	if pageSize < 0 {
		return status.Error(codes.InvalidArgument, "page_size must be non-negative")
	}
	return nil
}
