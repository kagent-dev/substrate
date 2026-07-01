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

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) CreateWorkerPoolGrant(ctx context.Context, req *ateapipb.CreateWorkerPoolGrantRequest) (*ateapipb.CreateWorkerPoolGrantResponse, error) {
	grant := req.GetGrant()
	if err := validateWorkerPoolGrant(grant); err != nil {
		return nil, err
	}

	exists, err := s.persistence.AtespaceExists(ctx, grant.GetAtespace())
	if err != nil {
		return nil, fmt.Errorf("while checking atespace: %w", err)
	}
	if !exists {
		return nil, status.Errorf(codes.FailedPrecondition, "Atespace %s not found", grant.GetAtespace())
	}

	wp := grant.GetWorkerPool()
	_, err = s.persistence.GetWorkerPool(ctx, wp.GetNamespace(), wp.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.FailedPrecondition, "WorkerPool %s/%s not found", wp.GetNamespace(), wp.GetName())
		}
		return nil, fmt.Errorf("while getting WorkerPool: %w", err)
	}

	if err := s.persistence.CreateWorkerPoolGrant(ctx, grant); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "WorkerPoolGrant %s/%s/%s already exists", grant.GetAtespace(), wp.GetNamespace(), wp.GetName())
		}
		return nil, fmt.Errorf("while recording worker pool grant: %w", err)
	}

	stored, err := s.persistence.GetWorkerPoolGrant(ctx, grant.GetAtespace(), wp.GetNamespace(), wp.GetName())
	if err != nil {
		return nil, fmt.Errorf("while fetching recorded worker pool grant from DB: %w", err)
	}
	return &ateapipb.CreateWorkerPoolGrantResponse{Grant: stored}, nil
}

func (s *Service) GetWorkerPoolGrant(ctx context.Context, req *ateapipb.GetWorkerPoolGrantRequest) (*ateapipb.GetWorkerPoolGrantResponse, error) {
	if err := validateWorkerPoolGrantRef(req.GetAtespace(), req.GetWorkerPool()); err != nil {
		return nil, err
	}
	wp := req.GetWorkerPool()
	grant, err := s.persistence.GetWorkerPoolGrant(ctx, req.GetAtespace(), wp.GetNamespace(), wp.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "WorkerPoolGrant %s/%s/%s not found", req.GetAtespace(), wp.GetNamespace(), wp.GetName())
		}
		return nil, fmt.Errorf("while getting worker pool grant from DB: %w", err)
	}
	return &ateapipb.GetWorkerPoolGrantResponse{Grant: grant}, nil
}

func (s *Service) ListWorkerPoolGrants(ctx context.Context, req *ateapipb.ListWorkerPoolGrantsRequest) (*ateapipb.ListWorkerPoolGrantsResponse, error) {
	if req.GetAtespace() != "" {
		if err := resources.ValidateAtespace(req.GetAtespace()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	grants, err := s.persistence.ListWorkerPoolGrants(ctx, req.GetAtespace())
	if err != nil {
		return nil, fmt.Errorf("while listing worker pool grants in db: %w", err)
	}
	return &ateapipb.ListWorkerPoolGrantsResponse{Grants: grants}, nil
}

func (s *Service) DeleteWorkerPoolGrant(ctx context.Context, req *ateapipb.DeleteWorkerPoolGrantRequest) (*ateapipb.DeleteWorkerPoolGrantResponse, error) {
	if err := validateWorkerPoolGrantRef(req.GetAtespace(), req.GetWorkerPool()); err != nil {
		return nil, err
	}
	wp := req.GetWorkerPool()
	if err := s.persistence.DeleteWorkerPoolGrant(ctx, req.GetAtespace(), wp.GetNamespace(), wp.GetName()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "WorkerPoolGrant %s/%s/%s not found", req.GetAtespace(), wp.GetNamespace(), wp.GetName())
		}
		return nil, fmt.Errorf("while deleting worker pool grant from DB: %w", err)
	}
	return &ateapipb.DeleteWorkerPoolGrantResponse{}, nil
}

func validateWorkerPoolGrant(grant *ateapipb.WorkerPoolGrant) error {
	if grant == nil {
		return status.Error(codes.InvalidArgument, "grant is required")
	}
	return validateWorkerPoolGrantRef(grant.GetAtespace(), grant.GetWorkerPool())
}

func validateWorkerPoolGrantRef(atespace string, workerPool *ateapipb.WorkerPoolRef) error {
	if atespace == "" {
		return status.Error(codes.InvalidArgument, "atespace is required")
	}
	if err := resources.ValidateAtespace(atespace); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if workerPool == nil {
		return status.Error(codes.InvalidArgument, "worker_pool is required")
	}
	return validateWorkerPoolRef(workerPool)
}
