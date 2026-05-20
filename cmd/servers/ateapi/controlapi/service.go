//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package controlapi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/agent-substrate/substrate/cmd/servers/ateapi/store"
	listersv1alpha1 "github.com/agent-substrate/substrate/pkg/client/listers/api/v1alpha1"

	"github.com/agent-substrate/substrate/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
)

// Service implements ateapipb.Control
type Service struct {
	ateapipb.UnimplementedControlServer
	persistence         store.Interface
	dialer              *AteletDialer
	actorTemplateLister listersv1alpha1.ActorTemplateLister
	actorWorkflow       *ActorWorkflow
}

var _ ateapipb.ControlServer = (*Service)(nil)

// NewService creates a service.
func NewService(persistence store.Interface, actorTemplateLister listersv1alpha1.ActorTemplateLister, dialer *AteletDialer, kubeClient kubernetes.Interface) *Service {
	s := &Service{
		persistence:         persistence,
		actorTemplateLister: actorTemplateLister,
		dialer:              dialer,
		actorWorkflow:       NewActorWorkflow(persistence, dialer, actorTemplateLister, kubeClient),
	}

	return s
}

// StatusErrorInterceptor searches the error chain for a gRPC status error.
// If found, it extracts the code and message to send to the client, ignoring
// any outer wrapping, while logging the full error chain internally.
func StatusErrorInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		var statusErr interface {
			GRPCStatus() *status.Status
		}

		if errors.As(err, &statusErr) {
			st := statusErr.GRPCStatus()
			slog.ErrorContext(ctx, "gRPC Error", slog.String("method", info.FullMethod), slog.Any("err", err))
			return nil, status.Error(st.Code(), st.Message())
		}

		slog.ErrorContext(ctx, "Unexpected error", slog.String("method", info.FullMethod), slog.Any("err", err))
		return nil, status.Error(codes.Internal, "internal server error")
	}
	return resp, nil
}
