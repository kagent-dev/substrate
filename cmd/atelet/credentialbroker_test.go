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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"testing"
	"time"

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
	"google.golang.org/protobuf/proto"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type brokerIdentityClient struct {
	ateapipb.ActorIdentityClient
	request *ateapipb.MintCertRequest
}

func (c *brokerIdentityClient) MintCert(_ context.Context, req *ateapipb.MintCertRequest, _ ...grpc.CallOption) (*ateapipb.MintCertResponse, error) {
	c.request = req
	return &ateapipb.MintCertResponse{ActorCertificates: [][]byte{{1, 2, 3}}}, nil
}

func TestCredentialBrokerForwardsAuthenticatedWorkerIdentity(t *testing.T) {
	identity := &brokerIdentityClient{}
	broker := &credentialBroker{actorIdentityClient: identity, workerAuth: mtlsWorkerAuthenticator{}}
	csr := []byte{4, 5, 6}
	resp, err := broker.MintActorCertificate(workerContext(t, "worker-uid"), &ateletpb.MintActorCertificateRequest{
		CertificateSigningRequest: csr,
		ExpectedActorUid:          "actor-uid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(resp, &ateletpb.MintActorCertificateResponse{ActorCertificates: [][]byte{{1, 2, 3}}}) {
		t.Fatalf("response = %+v", resp)
	}
	want := &ateapipb.MintCertRequest{WorkerNamespace: "workers", WorkerPod: "worker", WorkerPodUid: "worker-uid", ExpectedActorUid: "actor-uid", CertificateSigningRequest: csr, Purpose: ateapipb.ActorCertificatePurpose_ACTOR_CERTIFICATE_PURPOSE_ATUNNEL}
	if !proto.Equal(identity.request, want) {
		t.Fatalf("MintCert request = %+v, want %+v", identity.request, want)
	}
}

func workerContext(t *testing.T, podUID string) context.Context {
	t.Helper()
	cert := workerCertificate(t, podUID, "node")
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}}})
}

func TestVerifyClientOnSameNode(t *testing.T) {
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{workerCertificate(t, "worker-uid", "node-a")}}
	nodeA := &substratex509.PodIdentity{NodeName: "node-a", NodeUID: "node-uid"}
	if err := verifyClientOnSameNode(nodeA)(state); err != nil {
		t.Fatalf("same-node worker rejected: %v", err)
	}
	if err := verifyClientOnSameNode(&substratex509.PodIdentity{NodeName: "node-b", NodeUID: "node-uid"})(state); err == nil {
		t.Fatal("cross-node worker accepted")
	}
	if err := verifyClientOnSameNode(&substratex509.PodIdentity{NodeName: "node-a", NodeUID: "replacement-node"})(state); err == nil {
		t.Fatal("replacement node accepted")
	}
}

func TestValidateJWTWorkerClaims(t *testing.T) {
	valid := &k8sjwt.KubernetesClaims{
		Subject:            "system:serviceaccount:workers:default",
		Namespace:          "workers",
		ServiceAccountName: "default",
		ServiceAccountUID:  "sa-uid",
		PodUID:             "pod-uid",
		NodeName:           "node-a",
		NodeUID:            "node-uid",
	}
	if err := validateJWTWorkerClaims(valid, "node-a", "node-uid"); err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}
	for name, mutate := range map[string]func(*k8sjwt.KubernetesClaims){
		"unbound pod":     func(c *k8sjwt.KubernetesClaims) { c.PodUID = "" },
		"unbound node":    func(c *k8sjwt.KubernetesClaims) { c.NodeUID = "" },
		"wrong node":      func(c *k8sjwt.KubernetesClaims) { c.NodeUID = "other-node" },
		"wrong subject":   func(c *k8sjwt.KubernetesClaims) { c.Subject = "system:serviceaccount:workers:other" },
		"unbound account": func(c *k8sjwt.KubernetesClaims) { c.ServiceAccountUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			claims := *valid
			mutate(&claims)
			if err := validateJWTWorkerClaims(&claims, "node-a", "node-uid"); err == nil {
				t.Fatal("invalid claims accepted")
			}
		})
	}
}

func TestTokenReviewUnaryInterceptor(t *testing.T) {
	const subject = "system:serviceaccount:ate-system:ate-api-server"
	tests := []struct {
		name       string
		token      string
		status     authenticationv1.TokenReviewStatus
		wantCode   codes.Code
		wantCalled bool
	}{
		{name: "missing", wantCode: codes.Unauthenticated},
		{name: "expired", token: "token", status: authenticationv1.TokenReviewStatus{Authenticated: false}, wantCode: codes.Unauthenticated},
		{name: "wrong audience", token: "token", status: tokenReviewStatus(subject, "other", true), wantCode: codes.Unauthenticated},
		{name: "wrong role", token: "token", status: tokenReviewStatus("system:serviceaccount:ate-system:other", "atelet", true), wantCode: codes.PermissionDenied},
		{name: "unbound pod", token: "token", status: tokenReviewStatus(subject, "atelet", false), wantCode: codes.PermissionDenied},
		{name: "valid", token: "token", status: tokenReviewStatus(subject, "atelet", true), wantCalled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.token != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+tc.token))
			}
			called := false
			_, err := tokenReviewUnaryInterceptor(tokenReviewClient(t, tc.status), "atelet", subject)(ctx, nil, nil, func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
			if status.Code(err) != tc.wantCode || called != tc.wantCalled {
				t.Fatalf("code/called = %v/%v, want %v/%v", status.Code(err), called, tc.wantCode, tc.wantCalled)
			}
		})
	}
}

func TestJWTWorkerAuthenticator(t *testing.T) {
	valid := tokenReviewStatus("system:serviceaccount:workers:default", "atelet", true)
	valid.User.Extra["authentication.kubernetes.io/node-name"] = authenticationv1.ExtraValue{"node-a"}
	valid.User.Extra["authentication.kubernetes.io/node-uid"] = authenticationv1.ExtraValue{"node-uid"}
	tests := []struct {
		name     string
		token    string
		status   authenticationv1.TokenReviewStatus
		wantCode codes.Code
	}{
		{name: "missing", status: valid, wantCode: codes.Unauthenticated},
		{name: "expired", token: "token", status: authenticationv1.TokenReviewStatus{Authenticated: false}, wantCode: codes.Unauthenticated},
		{name: "wrong audience", token: "token", status: tokenReviewStatus("system:serviceaccount:workers:default", "other", true), wantCode: codes.Unauthenticated},
		{name: "wrong node", token: "token", status: tokenReviewStatus("system:serviceaccount:workers:default", "atelet", true), wantCode: codes.PermissionDenied},
		{name: "valid", token: "token", status: valid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.token != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+tc.token))
			}
			auth := jwtWorkerAuthenticator{client: tokenReviewClient(t, tc.status), audience: "atelet", nodeName: "node-a", nodeUID: "node-uid"}
			_, err := auth.authenticate(ctx)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v", status.Code(err), tc.wantCode)
			}
		})
	}
}

func tokenReviewStatus(subject, audience string, podBound bool) authenticationv1.TokenReviewStatus {
	extra := map[string]authenticationv1.ExtraValue{}
	if podBound {
		extra["authentication.kubernetes.io/pod-name"] = authenticationv1.ExtraValue{"pod"}
		extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"pod-uid"}
	}
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{audience},
		User:          authenticationv1.UserInfo{Username: subject, UID: "sa-uid", Extra: extra},
	}
}

func tokenReviewClient(t *testing.T, reviewStatus authenticationv1.TokenReviewStatus) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if review.Spec.Token != "token" || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != "atelet" {
			t.Fatalf("unexpected TokenReview spec: %+v", review.Spec)
		}
		return true, &authenticationv1.TokenReview{Status: reviewStatus}, nil
	})
	return client
}

func workerCertificate(t *testing.T, podUID, nodeName string) *x509.Certificate {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	if err := substratex509.AddPodIdentityToCertificate(&substratex509.PodIdentity{
		Namespace: "workers", ServiceAccountName: "default", ServiceAccountUID: "sa-uid",
		PodName: "worker", PodUID: podUID, NodeName: nodeName, NodeUID: "node-uid",
	}, template); err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
