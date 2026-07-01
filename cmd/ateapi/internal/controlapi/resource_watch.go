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
	"fmt"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func (s *Service) WatchActorTemplates(req *ateapipb.WatchActorTemplatesRequest, stream ateapipb.Control_WatchActorTemplatesServer) error {
	ctx := stream.Context()
	watch, err := s.persistence.WatchActorTemplates(ctx)
	if err != nil {
		return fmt.Errorf("while watching ActorTemplates: %w", err)
	}
	defer watch.Close()

	templates, err := s.persistence.ListActorTemplates(ctx, "")
	if err != nil {
		return fmt.Errorf("while listing initial ActorTemplates: %w", err)
	}
	for _, at := range templates {
		if err := stream.Send(&ateapipb.WatchActorTemplatesResponse{Type: ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_CREATED, ActorTemplate: at}); err != nil {
			return err
		}
	}
	for event := range watch.Events {
		if err := stream.Send(&ateapipb.WatchActorTemplatesResponse{Type: protoResourceEventType(event.Type), ActorTemplate: event.ActorTemplate}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) WatchWorkerPools(req *ateapipb.WatchWorkerPoolsRequest, stream ateapipb.Control_WatchWorkerPoolsServer) error {
	ctx := stream.Context()
	watch, err := s.persistence.WatchWorkerPools(ctx)
	if err != nil {
		return fmt.Errorf("while watching WorkerPools: %w", err)
	}
	defer watch.Close()

	pools, err := s.persistence.ListWorkerPools(ctx)
	if err != nil {
		return fmt.Errorf("while listing initial WorkerPools: %w", err)
	}
	for _, wp := range pools {
		if err := stream.Send(&ateapipb.WatchWorkerPoolsResponse{Type: ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_CREATED, WorkerPool: wp}); err != nil {
			return err
		}
	}
	for event := range watch.Events {
		if err := stream.Send(&ateapipb.WatchWorkerPoolsResponse{Type: protoResourceEventType(event.Type), WorkerPool: event.WorkerPool}); err != nil {
			return err
		}
	}
	return nil
}

func protoResourceEventType(t store.ResourceEventType) ateapipb.ResourceEventType {
	switch t {
	case store.ResourceEventCreated:
		return ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_CREATED
	case store.ResourceEventUpdated:
		return ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_UPDATED
	case store.ResourceEventDeleted:
		return ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_DELETED
	default:
		return ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_UNSPECIFIED
	}
}
