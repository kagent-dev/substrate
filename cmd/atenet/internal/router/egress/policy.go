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
	"encoding/base64"
	"net"
	"net/netip"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/internal/proto/egresspolicypb"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

const (
	inspectionHeader    = "x-ate-egress-inspect"
	policyContextHeader = "x-ate-egress-context"
)

func actorRef(identity *substratex509.ActorIdentity) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: identity.Atespace, Name: identity.ActorName}
}

// routePolicy decides at CONNECT time whether the original destination IP is
// already allowed or whether Envoy must inspect HTTP authority/TLS SNI.
func routePolicy(effective *egresspolicypb.EffectiveEgressPolicy, destination string) (bool, error) {
	policy := effective.GetPolicy()
	if policy == nil || len(policy.GetExtensions()) != 0 {
		return false, extproc.NewReqError(envoy_type.StatusCode_Forbidden, "egress denied by policy")
	}
	host, _, err := net.SplitHostPort(destination)
	if err != nil {
		return false, extproc.NewReqError(envoy_type.StatusCode_Forbidden, "egress denied: invalid destination")
	}
	if hasCredentialInjection(policy) {
		return true, nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && allowsIP(policy, ip) {
		return false, nil
	}
	for _, rule := range policy.GetRules() {
		if rule.GetHostname() != nil {
			return true, nil
		}
	}
	return false, extproc.NewReqError(envoy_type.StatusCode_Forbidden, "egress denied by policy")
}

func allowsIP(policy *ateapipb.EgressPolicy, ip netip.Addr) bool {
	if policy.GetAllowAll() != nil {
		return true
	}
	for _, rule := range policy.GetRules() {
		if match, ok := rule.GetMatch().(*ateapipb.EgressRule_IpBlocks); ok {
			for _, cidr := range match.IpBlocks.GetCidrs() {
				prefix, err := netip.ParsePrefix(cidr)
				if err == nil && prefix.Contains(ip) {
					return true
				}
			}
		}
	}
	return false
}

func hasCredentialInjection(policy *ateapipb.EgressPolicy) bool {
	for _, rule := range policy.GetRules() {
		if rule.GetHostname().GetCredentialInjection() != nil {
			return true
		}
	}
	return false
}

func hostnameMatches(policy *ateapipb.EgressPolicy, hostname string) []*ateapipb.HostnameMatch {
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	var matches []*ateapipb.HostnameMatch
	for _, rule := range policy.GetRules() {
		match := rule.GetHostname()
		if match == nil {
			continue
		}
		pattern := match.GetPattern()
		if pattern == hostname {
			matches = append(matches, match)
			continue
		}
		if suffix, ok := strings.CutPrefix(pattern, "*."); ok && strings.HasSuffix(hostname, "."+suffix) {
			label := strings.TrimSuffix(hostname, "."+suffix)
			if label != "" && !strings.Contains(label, ".") {
				matches = append(matches, match)
			}
		}
	}
	return matches
}

func policyContext(effective *egresspolicypb.EffectiveEgressPolicy, destination string) string {
	b, err := proto.Marshal(effective)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString([]byte(destination))
}

func policyFromCheck(req *authv3.CheckRequest) (*egresspolicypb.EffectiveEgressPolicy, string, bool) {
	httpReq := req.GetAttributes().GetRequest().GetHttp()
	parts := strings.Split(httpReq.GetHeaders()[policyContextHeader], ".")
	if len(parts) != 2 {
		return nil, "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, "", false
	}
	destination, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, "", false
	}
	effective := &egresspolicypb.EffectiveEgressPolicy{}
	if err := proto.Unmarshal(b, effective); err != nil {
		return nil, "", false
	}
	return effective, string(destination), true
}

// Check enforces HTTP authority and credential policy passed from the
// authenticated CONNECT to the internal listener.
func (h *Handler) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	effective, destination, ok := policyFromCheck(req)
	if !ok || len(effective.GetPolicy().GetExtensions()) != 0 {
		return denied(), nil
	}

	hostname := ""
	if httpReq := req.GetAttributes().GetRequest().GetHttp(); httpReq != nil {
		hostname = httpReq.GetHost()
		if host, _, err := net.SplitHostPort(hostname); err == nil {
			hostname = host
		}
	}
	matches := hostnameMatches(effective.GetPolicy(), hostname)
	if len(matches) == 0 {
		host, _, err := net.SplitHostPort(destination)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || !allowsIP(effective.GetPolicy(), ip) {
			return denied(), nil
		}
	}

	okResponse := &authv3.OkHttpResponse{HeadersToRemove: []string{policyContextHeader}}
	if req.GetAttributes().GetRequest().GetHttp() != nil && len(matches) != 0 {
		credentials := map[string][]byte{}
		for _, credential := range effective.GetCredentials() {
			credentials[credential.GetName()] = credential.GetValue()
		}
		for _, match := range matches {
			injection := match.GetCredentialInjection()
			if injection == nil {
				continue
			}
			value, found := credentials[injection.GetCredential().GetName()]
			if !found {
				return denied(), nil
			}
			okResponse.Headers = append(okResponse.Headers, &corev3.HeaderValueOption{Header: &corev3.HeaderValue{
				Key: injection.GetHeader(), RawValue: value,
			}})
		}
	}
	return &authv3.CheckResponse{
		Status:       &statuspb.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: okResponse},
	}, nil
}

func denied() *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &statuspb.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status: &envoy_type.HttpStatus{Code: envoy_type.StatusCode_Forbidden},
		}},
	}
}
