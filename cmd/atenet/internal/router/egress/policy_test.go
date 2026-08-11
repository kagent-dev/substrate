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

package egress

import (
	"context"
	"testing"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-substrate/substrate/internal/proto/egresspolicypb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestPolicyRoutingAndInjection(t *testing.T) {
	effective := &egresspolicypb.EffectiveEgressPolicy{
		Policy: &ateapipb.EgressPolicy{AllowAll: &emptypb.Empty{}, Rules: []*ateapipb.EgressRule{
			{Match: &ateapipb.EgressRule_Hostname{Hostname: &ateapipb.HostnameMatch{
				Pattern: "api.example.com",
				CredentialInjection: &ateapipb.HeaderCredentialInjection{
					Header: "authorization", Credential: &ateapipb.CredentialReference{Name: "token"},
				},
			}}},
		}},
		Credentials: []*egresspolicypb.ResolvedCredential{{Name: "token", Value: []byte("Bearer secret")}},
	}

	inspect, err := routePolicy(effective, "203.0.113.4:443")
	if err != nil || !inspect {
		t.Fatalf("allow-all route with injection = inspect %v, err %v; want inspection", inspect, err)
	}

	resp, err := (&Handler{}).Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Host: "api.example.com", Headers: map[string]string{policyContextHeader: policyContext(effective, "203.0.113.4:443")},
		}},
	}})
	if err != nil || resp.GetStatus().GetCode() != int32(codes.OK) {
		t.Fatalf("HTTP policy check = %v, %v; want allow", resp, err)
	}
	headers := resp.GetOkResponse().GetHeaders()
	if len(headers) != 1 || string(headers[0].GetHeader().GetRawValue()) != "Bearer secret" {
		t.Fatalf("injected headers = %v", headers)
	}

	resp, err = (&Handler{}).Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Host: "other.example.com", Headers: map[string]string{policyContextHeader: policyContext(effective, "203.0.113.4:443")},
		}},
	}})
	if err != nil || resp.GetStatus().GetCode() != int32(codes.OK) || len(resp.GetOkResponse().GetHeaders()) != 0 {
		t.Fatalf("non-matching allow-all request = %v, %v; want allow without injection", resp, err)
	}

	direct := &egresspolicypb.EffectiveEgressPolicy{Policy: &ateapipb.EgressPolicy{Rules: []*ateapipb.EgressRule{{
		Match: &ateapipb.EgressRule_IpBlocks{IpBlocks: &ateapipb.IPBlockMatch{Cidrs: []string{"192.0.2.0/24"}}},
	}}}}
	inspect, err = routePolicy(direct, "192.0.2.4:443")
	if err != nil || inspect {
		t.Fatalf("CIDR route = inspect %v, err %v; want direct", inspect, err)
	}
}

func TestPolicyCheckFailsClosed(t *testing.T) {
	resp, err := (&Handler{}).Check(context.Background(), &authv3.CheckRequest{})
	if err != nil || resp.GetStatus().GetCode() != int32(codes.PermissionDenied) {
		t.Fatalf("missing policy check = %v, %v; want deny", resp, err)
	}
}
