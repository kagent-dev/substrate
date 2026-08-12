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

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/internal/k8sjwt"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
)

func tokenReviewUnaryInterceptor(client kubernetes.Interface, audience, subject string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		claims, err := k8sjwt.TokenReview(ctx, client, strings.TrimSpace(strings.TrimPrefix(values[0], "Bearer ")), audience)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
		}
		if claims.Subject != subject || claims.PodUID == "" {
			return nil, status.Error(codes.PermissionDenied, "caller is not permitted")
		}
		return handler(ctx, req)
	}
}

type credentialBroker struct {
	ateletpb.UnimplementedCredentialBrokerServer
	// actorIdentityClient resolves the authenticated worker's current assignment
	// and signs its actor certificate.
	actorIdentityClient ateapipb.ActorIdentityClient
	tokenAuth           *brokerTokenAuthenticator
}

type brokerTokenAuthenticator struct {
	client            kubernetes.Interface
	audience          string
	nodeName, nodeUID string
}

func (b *credentialBroker) MintActorCertificate(ctx context.Context, req *ateletpb.MintActorCertificateRequest) (*ateletpb.MintActorCertificateResponse, error) {
	// TODO: Before release, require the egress PEP to reject actor certificates
	// whose ActorIdentity purpose is not atunnel.
	// Worker identity comes only from the mTLS certificate. The expected actor
	// UID is a stale-activation guard; ateapi derives the actor authoritatively.
	workerIdentity, err := b.authenticatedWorkerIdentity(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := b.actorIdentityClient.MintCert(ctx, &ateapipb.MintCertRequest{
		WorkerNamespace:           workerIdentity.Namespace,
		WorkerPod:                 workerIdentity.PodName,
		WorkerPodUid:              workerIdentity.PodUID,
		ExpectedActorUid:          req.GetExpectedActorUid(),
		CertificateSigningRequest: req.GetCertificateSigningRequest(),
		Purpose:                   ateapipb.ActorCertificatePurpose_ACTOR_CERTIFICATE_PURPOSE_ATUNNEL,
	})
	if err != nil {
		return nil, fmt.Errorf("mint actor certificate: %w", err)
	}
	return &ateletpb.MintActorCertificateResponse{ActorCertificates: resp.GetActorCertificates()}, nil
}

func (b *credentialBroker) authenticatedWorkerIdentity(ctx context.Context) (*substratex509.PodIdentity, error) {
	if b.tokenAuth != nil {
		return b.tokenAuth.authenticate(ctx)
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing peer credentials")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing peer certificate")
	}
	identity, err := substratex509.PodIdentityFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil || identity == nil {
		return nil, status.Error(codes.PermissionDenied, "invalid worker identity")
	}
	return identity, nil
}

func (a *brokerTokenAuthenticator) authenticate(ctx context.Context) (*substratex509.PodIdentity, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("authorization")) != 1 {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	authorization := md.Get("authorization")[0]
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	reviewed, err := k8sjwt.TokenReview(ctx, a.client, strings.TrimSpace(authorization[len(prefix):]), a.audience)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	claims := &k8sjwt.KubernetesClaims{
		Subject: reviewed.Subject, Namespace: reviewed.Namespace, ServiceAccountName: reviewed.ServiceAccountName,
		ServiceAccountUID: reviewed.ServiceAccountUID, PodName: reviewed.PodName, PodUID: reviewed.PodUID,
		NodeName: reviewed.NodeName, NodeUID: reviewed.NodeUID,
	}
	if err := validateBrokerTokenClaims(claims, a.nodeName, a.nodeUID); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &substratex509.PodIdentity{
		Namespace: claims.Namespace, ServiceAccountName: claims.ServiceAccountName, ServiceAccountUID: claims.ServiceAccountUID,
		PodName: claims.PodName, PodUID: claims.PodUID, NodeName: claims.NodeName, NodeUID: claims.NodeUID,
	}, nil
}

func validateBrokerTokenClaims(claims *k8sjwt.KubernetesClaims, nodeName, nodeUID string) error {
	if claims.PodUID == "" || claims.NodeUID == "" || claims.ServiceAccountUID == "" {
		return fmt.Errorf("token is not bound to a Pod, node, and ServiceAccount")
	}
	if claims.NodeName != nodeName || claims.NodeUID != nodeUID {
		return fmt.Errorf("worker is not on this node")
	}
	wantSubject := "system:serviceaccount:" + claims.Namespace + ":" + claims.ServiceAccountName
	if claims.Namespace == "" || claims.ServiceAccountName == "" || claims.Subject != wantSubject {
		return fmt.Errorf("invalid ServiceAccount identity")
	}
	return nil
}

// verifyClientOnSameNode returns a TLS callback that accepts only worker Pods
// scheduled on the atelet's node incarnation.
func verifyClientOnSameNode(node *substratex509.PodIdentity) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("worker certificate is required")
		}
		identity, err := substratex509.PodIdentityFromCertificate(state.PeerCertificates[0])
		if err != nil {
			return fmt.Errorf("parse worker Pod identity: %w", err)
		}
		if identity == nil || identity.NodeName != node.NodeName || identity.NodeUID != node.NodeUID {
			return fmt.Errorf("worker is not on node %q (%s)", node.NodeName, node.NodeUID)
		}
		return nil
	}
}
