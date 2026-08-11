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
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/principal"
	"github.com/agent-substrate/substrate/internal/proto/egresspolicypb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TODO: Make this configurable when Substrate supports installing the egress gateway outside ate-system.
const egressGatewayPrincipal = "spiffe://cluster.local/ns/ate-system/sa/atenet-egress"

var (
	egressPolicyMutableFields = mutableFields[*ateapipb.EgressPolicy]{
		"allow_all": func(dst, src *ateapipb.EgressPolicy) {
			dst.AllowAll = nil
			if src.GetAllowAll() != nil {
				dst.AllowAll = &emptypb.Empty{}
			}
		},
		"rules": func(dst, src *ateapipb.EgressPolicy) {
			dst.Rules = proto.Clone(src).(*ateapipb.EgressPolicy).GetRules()
		},
		"extensions": func(dst, src *ateapipb.EgressPolicy) {
			dst.Extensions = proto.Clone(src).(*ateapipb.EgressPolicy).GetExtensions()
		},
	}
	credentialMutableFields = mutableFields[*ateapipb.Credential]{
		"kubernetes_secret": func(dst, src *ateapipb.Credential) {
			dst.Source = &ateapipb.Credential_KubernetesSecret{KubernetesSecret: proto.Clone(src.GetKubernetesSecret()).(*ateapipb.KubernetesSecretKeySelector)}
		},
	}
)

func (s *Service) GetEgressPolicy(ctx context.Context, req *ateapipb.GetEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	ref := req.GetEgressPolicy()
	if errs := validateScopedRef(ref, field.NewPath("egress_policy")); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	policy, err := s.persistence.GetEgressPolicy(ctx, ref.GetAtespace(), ref.GetName())
	return getResource(policy, err, "EgressPolicy", ref)
}

func (s *Service) CreateEgressPolicy(ctx context.Context, req *ateapipb.CreateEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	policy := req.GetEgressPolicy()
	if errs := validateEgressPolicy(policy); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	policy = normalizeEgressPolicy(policy)
	actorRef := resources.ActorRefFromObjectRef(policy.GetActor())
	actor, err := s.persistence.GetActor(ctx, actorRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.FailedPrecondition, "target Actor %s/%s does not exist", actorRef.Atespace, actorRef.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("while resolving target Actor: %w", err)
	}
	created, err := s.persistence.CreateEgressPolicy(ctx, policy, actor.GetMetadata().GetUid())
	return mapResourceWrite(created, err, "EgressPolicy")
}

func (s *Service) UpdateEgressPolicy(ctx context.Context, req *ateapipb.UpdateEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	policy := req.GetEgressPolicy()
	errs := validateEgressPolicy(policy)
	errs = append(errs, validateUpdateMask(req.GetUpdateMask(), egressPolicyMutableFields)...)
	if len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	policy = normalizeEgressPolicy(policy)
	md := policy.GetMetadata()
	updated, err := s.persistence.UpdateEgressPolicy(ctx, md.GetAtespace(), md.GetName(), func(current *ateapipb.EgressPolicy) error {
		if err := checkMetadataPreconditions(current.GetMetadata(), md); err != nil {
			return err
		}
		if !proto.Equal(current.GetActor(), policy.GetActor()) {
			return store.ErrFailedPrecondition
		}
		applyUpdateMask(current, policy, req.GetUpdateMask(), egressPolicyMutableFields)
		return nil
	})
	return mapResourceWrite(updated, err, "EgressPolicy")
}

func (s *Service) DeleteEgressPolicy(ctx context.Context, req *ateapipb.DeleteEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	ref := req.GetEgressPolicy()
	if errs := validateScopedRef(ref, field.NewPath("egress_policy")); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	deleted, err := s.persistence.DeleteEgressPolicy(ctx, ref.GetAtespace(), ref.GetName())
	return mapResourceWrite(deleted, err, "EgressPolicy")
}

func (s *Service) ListEgressPolicies(ctx context.Context, req *ateapipb.ListEgressPoliciesRequest) (*ateapipb.ListEgressPoliciesResponse, error) {
	if errs := validateScopedList(req.GetAtespace(), req.GetPageSize()); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	policies, next, err := s.persistence.ListEgressPolicies(ctx, req.GetAtespace(), effectivePageSize(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, fmt.Errorf("while listing egress policies: %w", err)
	}
	return &ateapipb.ListEgressPoliciesResponse{EgressPolicies: policies, NextPageToken: next}, nil
}

func (s *Service) GetCredential(ctx context.Context, req *ateapipb.GetCredentialRequest) (*ateapipb.Credential, error) {
	ref := req.GetCredential()
	if errs := validateScopedRef(ref, field.NewPath("credential")); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	credential, err := s.persistence.GetCredential(ctx, ref.GetAtespace(), ref.GetName())
	return getResource(credential, err, "Credential", ref)
}

func (s *Service) CreateCredential(ctx context.Context, req *ateapipb.CreateCredentialRequest) (*ateapipb.Credential, error) {
	credential := req.GetCredential()
	if errs := validateCredential(credential); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	atespace := credential.GetMetadata().GetAtespace()
	if _, err := s.persistence.GetAtespace(ctx, atespace); errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.FailedPrecondition, "Atespace %s does not exist", atespace)
	} else if err != nil {
		return nil, fmt.Errorf("while resolving Atespace: %w", err)
	}
	created, err := s.persistence.CreateCredential(ctx, credential)
	return mapResourceWrite(created, err, "Credential")
}

func (s *Service) UpdateCredential(ctx context.Context, req *ateapipb.UpdateCredentialRequest) (*ateapipb.Credential, error) {
	credential := req.GetCredential()
	errs := validateCredential(credential)
	errs = append(errs, validateUpdateMask(req.GetUpdateMask(), credentialMutableFields)...)
	if len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	md := credential.GetMetadata()
	updated, err := s.persistence.UpdateCredential(ctx, md.GetAtespace(), md.GetName(), func(current *ateapipb.Credential) error {
		if err := checkMetadataPreconditions(current.GetMetadata(), md); err != nil {
			return err
		}
		applyUpdateMask(current, credential, req.GetUpdateMask(), credentialMutableFields)
		return nil
	})
	return mapResourceWrite(updated, err, "Credential")
}

func (s *Service) DeleteCredential(ctx context.Context, req *ateapipb.DeleteCredentialRequest) (*ateapipb.Credential, error) {
	ref := req.GetCredential()
	if errs := validateScopedRef(ref, field.NewPath("credential")); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	deleted, err := s.persistence.DeleteCredential(ctx, ref.GetAtespace(), ref.GetName())
	return mapResourceWrite(deleted, err, "Credential")
}

func (s *Service) ListCredentials(ctx context.Context, req *ateapipb.ListCredentialsRequest) (*ateapipb.ListCredentialsResponse, error) {
	if errs := validateScopedList(req.GetAtespace(), req.GetPageSize()); len(errs) > 0 {
		return nil, toGRPCStatusError(errs)
	}
	credentials, next, err := s.persistence.ListCredentials(ctx, req.GetAtespace(), effectivePageSize(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, fmt.Errorf("while listing credentials: %w", err)
	}
	return &ateapipb.ListCredentialsResponse{Credentials: credentials, NextPageToken: next}, nil
}

func (s *Service) GetEffectiveEgressPolicy(ctx context.Context, req *egresspolicypb.GetEffectiveEgressPolicyRequest) (*egresspolicypb.EffectiveEgressPolicy, error) {
	p, ok := principal.FromContext(ctx)
	if !ok || p.Kind != principal.KindMTLS || p.ID != egressGatewayPrincipal {
		return nil, status.Error(codes.PermissionDenied, "caller is not the egress gateway")
	}
	if errs := validateScopedRef(req.GetActor(), field.NewPath("actor")); len(errs) > 0 || req.GetActorUid() == "" {
		if req.GetActorUid() == "" {
			errs = append(errs, field.Required(field.NewPath("actor_uid"), ""))
		}
		return nil, toGRPCStatusError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	actor, err := s.persistence.GetActor(ctx, actorRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.PermissionDenied, "actor is not authorized for egress")
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "while resolving actor: %v", err)
	}
	if actor.GetMetadata().GetUid() != req.GetActorUid() || actor.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
		return nil, status.Error(codes.PermissionDenied, "actor is not authorized for egress")
	}
	policy, err := s.persistence.GetEgressPolicyForActor(ctx, actorRef, req.GetActorUid())
	if errors.Is(err, store.ErrNotFound) {
		return &egresspolicypb.EffectiveEgressPolicy{Policy: &ateapipb.EgressPolicy{}}, nil
	}
	if errors.Is(err, store.ErrUIDConflict) {
		return nil, status.Error(codes.PermissionDenied, "actor is not authorized for this egress policy")
	}
	if err != nil {
		return nil, fmt.Errorf("while resolving egress policy: %w", err)
	}
	response := &egresspolicypb.EffectiveEgressPolicy{Policy: proto.Clone(policy).(*ateapipb.EgressPolicy)}
	for _, name := range referencedCredentials(policy) {
		credential, err := s.persistence.GetCredential(ctx, actorRef.Atespace, name)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "credential %q cannot be resolved", name)
		}
		selector := credential.GetKubernetesSecret()
		if selector == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "credential %q has no Kubernetes secret selector", name)
		}
		secret, err := s.kubeClient.CoreV1().Secrets(selector.GetNamespace()).Get(ctx, selector.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "credential %q cannot be resolved", name)
		}
		value, ok := secret.Data[selector.GetKey()]
		if !ok || !validHeaderValue(value) {
			return nil, status.Errorf(codes.FailedPrecondition, "credential %q has no valid value", name)
		}
		response.Credentials = append(response.Credentials, &egresspolicypb.ResolvedCredential{Name: name, Value: slices.Clone(value)})
	}
	return response, nil
}

func validateEgressPolicy(policy *ateapipb.EgressPolicy) field.ErrorList {
	root := field.NewPath("egress_policy")
	if policy == nil {
		return field.ErrorList{field.Required(root, "")}
	}
	var errs field.ErrorList
	md := policy.GetMetadata()
	errs = append(errs, resources.ValidateResourceMetadataRef(md, root.Child("metadata"))...)
	actor := policy.GetActor()
	errs = append(errs, validateScopedRef(actor, root.Child("actor"))...)
	if md != nil && actor != nil && md.GetAtespace() != actor.GetAtespace() {
		errs = append(errs, field.Invalid(root.Child("actor", "atespace"), actor.GetAtespace(), "must equal policy atespace"))
	}
	if policy.GetTarget() == nil {
		errs = append(errs, field.Required(root.Child("target"), ""))
	}
	seenHostnames := map[string]bool{}
	for i, rule := range policy.GetRules() {
		p := root.Child("rules").Index(i)
		switch match := rule.GetMatch().(type) {
		case *ateapipb.EgressRule_Hostname:
			errs = append(errs, validateHostnameMatch(match.Hostname, p.Child("hostname"))...)
			pattern := strings.ToLower(strings.TrimSuffix(match.Hostname.GetPattern(), "."))
			if pattern != "" && seenHostnames[pattern] {
				errs = append(errs, field.Duplicate(p.Child("hostname", "pattern"), match.Hostname.GetPattern()))
			}
			if pattern != "" {
				seenHostnames[pattern] = true
			}
		case *ateapipb.EgressRule_IpBlocks:
			if len(match.IpBlocks.GetCidrs()) == 0 {
				errs = append(errs, field.Required(p.Child("ip_blocks", "cidrs"), ""))
			}
			for j, cidr := range match.IpBlocks.GetCidrs() {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil || prefix.Masked().String() != cidr {
					errs = append(errs, field.Invalid(p.Child("ip_blocks", "cidrs").Index(j), cidr, "must be a canonical IPv4 or IPv6 prefix"))
				}
			}
		default:
			errs = append(errs, field.Required(p.Child("match"), ""))
		}
	}
	return errs
}

func validateHostnameMatch(match *ateapipb.HostnameMatch, p *field.Path) field.ErrorList {
	if match == nil {
		return field.ErrorList{field.Required(p, "")}
	}
	pattern := strings.ToLower(strings.TrimSuffix(match.GetPattern(), "."))
	wildcard := strings.HasPrefix(pattern, "*.")
	name := strings.TrimPrefix(pattern, "*.")
	var errs field.ErrorList
	if pattern == "" || len(validation.IsDNS1123Subdomain(name)) != 0 || (strings.Contains(pattern, "*") && !wildcard) {
		errs = append(errs, field.Invalid(p.Child("pattern"), match.GetPattern(), "must be an exact DNS hostname or single-label wildcard"))
	}
	injection := match.GetCredentialInjection()
	if wildcard && injection != nil {
		errs = append(errs, field.Invalid(p.Child("credential_injection"), injection, "credential injection requires an exact hostname"))
	}
	if injection != nil {
		ip := p.Child("credential_injection")
		header := strings.ToLower(injection.GetHeader())
		if !validHeaderName(header) {
			errs = append(errs, field.Invalid(ip.Child("header"), injection.GetHeader(), "must be an HTTP header name"))
		}
		if injection.GetCredential().GetName() == "" {
			errs = append(errs, field.Required(ip.Child("credential", "name"), ""))
		} else {
			errs = append(errs, resources.ValidateResourceName(injection.GetCredential().GetName(), ip.Child("credential", "name"))...)
		}
	}
	return errs
}

func validateCredential(credential *ateapipb.Credential) field.ErrorList {
	root := field.NewPath("credential")
	if credential == nil {
		return field.ErrorList{field.Required(root, "")}
	}
	var errs field.ErrorList
	errs = append(errs, resources.ValidateResourceMetadataRef(credential.GetMetadata(), root.Child("metadata"))...)
	selector := credential.GetKubernetesSecret()
	if selector == nil {
		return append(errs, field.Required(root.Child("kubernetes_secret"), ""))
	}
	if selector.GetNamespace() == "" {
		errs = append(errs, field.Required(root.Child("kubernetes_secret", "namespace"), ""))
	} else if messages := validation.IsDNS1123Label(selector.GetNamespace()); len(messages) != 0 {
		errs = append(errs, field.Invalid(root.Child("kubernetes_secret", "namespace"), selector.GetNamespace(), strings.Join(messages, "; ")))
	}
	if selector.GetName() == "" {
		errs = append(errs, field.Required(root.Child("kubernetes_secret", "name"), ""))
	} else if messages := validation.IsDNS1123Subdomain(selector.GetName()); len(messages) != 0 {
		errs = append(errs, field.Invalid(root.Child("kubernetes_secret", "name"), selector.GetName(), strings.Join(messages, "; ")))
	}
	if selector.GetKey() == "" {
		errs = append(errs, field.Required(root.Child("kubernetes_secret", "key"), ""))
	}
	return errs
}

func normalizeEgressPolicy(policy *ateapipb.EgressPolicy) *ateapipb.EgressPolicy {
	result := proto.Clone(policy).(*ateapipb.EgressPolicy)
	for _, rule := range result.GetRules() {
		if hostname := rule.GetHostname(); hostname != nil {
			hostname.Pattern = strings.ToLower(strings.TrimSuffix(hostname.GetPattern(), "."))
			if injection := hostname.GetCredentialInjection(); injection != nil {
				injection.Header = strings.ToLower(injection.GetHeader())
			}
		}
	}
	return result
}

func validateScopedRef(ref *ateapipb.ObjectRef, p *field.Path) field.ErrorList {
	if ref == nil {
		return field.ErrorList{field.Required(p, "")}
	}
	return resources.ValidateObjectRef(ref, p)
}

func validateScopedList(atespace string, pageSize int32) field.ErrorList {
	var errs field.ErrorList
	if atespace != "" {
		errs = append(errs, resources.ValidateResourceName(atespace, field.NewPath("atespace"))...)
	}
	if pageSize < 0 {
		errs = append(errs, field.Invalid(field.NewPath("page_size"), pageSize, "must be greater than or equal to 0"))
	}
	return errs
}

func checkMetadataPreconditions(current, supplied *ateapipb.ResourceMetadata) error {
	if supplied.GetUid() != "" && supplied.GetUid() != current.GetUid() {
		return store.ErrUIDConflict
	}
	if supplied.GetVersion() != 0 && supplied.GetVersion() != current.GetVersion() {
		return store.ErrVersionConflict
	}
	return nil
}

func getResource[T any](resource T, err error, kind string, ref *ateapipb.ObjectRef) (T, error) {
	if errors.Is(err, store.ErrNotFound) {
		var zero T
		return zero, status.Errorf(codes.NotFound, "%s %s/%s not found", kind, ref.GetAtespace(), ref.GetName())
	}
	if err != nil {
		var zero T
		return zero, fmt.Errorf("while getting %s: %w", kind, err)
	}
	return resource, nil
}

func mapResourceWrite[T any](resource T, err error, kind string) (T, error) {
	var zero T
	switch {
	case err == nil:
		return resource, nil
	case errors.Is(err, store.ErrNotFound):
		return zero, status.Errorf(codes.NotFound, "%s not found", kind)
	case errors.Is(err, store.ErrAlreadyExists):
		return zero, status.Errorf(codes.AlreadyExists, "%s already exists", kind)
	case errors.Is(err, store.ErrUIDConflict), errors.Is(err, store.ErrVersionConflict):
		return zero, status.Errorf(codes.Aborted, "%s write precondition failed", kind)
	case errors.Is(err, store.ErrFailedPrecondition):
		return zero, status.Errorf(codes.FailedPrecondition, "%s target is immutable", kind)
	default:
		return zero, fmt.Errorf("while writing %s: %w", kind, err)
	}
}

func referencedCredentials(policy *ateapipb.EgressPolicy) []string {
	set := map[string]bool{}
	for _, rule := range policy.GetRules() {
		if injection := rule.GetHostname().GetCredentialInjection(); injection != nil {
			set[injection.GetCredential().GetName()] = true
		}
	}
	keys := mapsKeys(set)
	sort.Strings(keys)
	return keys
}

func mapsKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range []byte(value) {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func validHeaderValue(value []byte) bool {
	for _, c := range value {
		if c == '\r' || c == '\n' || c == 0 {
			return false
		}
	}
	return true
}
