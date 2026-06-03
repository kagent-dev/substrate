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
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

func buildAteletWorkloadSpec(actorTemplate *atev1alpha1.ActorTemplate) *ateletpb.WorkloadSpec {
	workloadSpec := &ateletpb.WorkloadSpec{
		PauseImage:   actorTemplate.Spec.PauseImage,
		EgressPolicy: buildAteletEgressPolicy(actorTemplate.Spec.EgressPolicy),
	}
	for _, ctr := range actorTemplate.Spec.Containers {
		ateletCtr := &ateletpb.Container{
			Name:    ctr.Name,
			Image:   ctr.Image,
			Command: ctr.Command,
		}
		for _, env := range ctr.Env {
			ateletCtr.Env = append(ateletCtr.Env, &ateletpb.EnvEntry{
				Name:  env.Name,
				Value: env.Value,
			})
		}
		workloadSpec.Containers = append(workloadSpec.Containers, ateletCtr)
	}
	return workloadSpec
}

func buildAteletEgressPolicy(policy *atev1alpha1.EgressPolicy) *ateletpb.EgressPolicy {
	if policy == nil {
		return nil
	}
	return &ateletpb.EgressPolicy{
		DefaultAction: string(policy.DefaultAction),
		Allow:         buildAteletEgressPolicyRules(policy.Allow),
		Deny:          buildAteletEgressPolicyRules(policy.Deny),
	}
}

func buildAteletEgressPolicyRules(rules []atev1alpha1.EgressPolicyRule) []*ateletpb.EgressPolicyRule {
	out := make([]*ateletpb.EgressPolicyRule, 0, len(rules))
	for _, rule := range rules {
		outRule := &ateletpb.EgressPolicyRule{}
		for _, dest := range rule.To {
			outDest := &ateletpb.EgressPolicyDestination{Host: dest.Host}
			if dest.IPBlock != nil {
				outDest.Cidr = dest.IPBlock.CIDR
			}
			outRule.To = append(outRule.To, outDest)
		}
		for _, port := range rule.Ports {
			outRule.Ports = append(outRule.Ports, &ateletpb.EgressPort{
				Port:     uint32(port.Port),
				Protocol: string(port.Protocol),
			})
		}
		outRule.Tls = buildAteletEgressTLSPolicy(rule.TLS)
		outRule.Credentials = buildAteletEgressCredentialPolicy(rule.Credentials)
		out = append(out, outRule)
	}
	return out
}

func buildAteletEgressTLSPolicy(policy *atev1alpha1.EgressTLSPolicy) *ateletpb.EgressTLSPolicy {
	if policy == nil {
		return nil
	}
	out := &ateletpb.EgressTLSPolicy{
		Mode:     string(policy.Mode),
		Required: policy.Required,
	}
	if policy.Intercept != nil {
		out.Intercept = &ateletpb.EgressTLSInterceptPolicy{
			ValidateUpstream: policy.Intercept.ValidateUpstream,
		}
		if policy.Intercept.IssuerSecretRef != nil {
			out.Intercept.IssuerSecretRef = &ateletpb.SecretReference{
				Name:      policy.Intercept.IssuerSecretRef.Name,
				Namespace: policy.Intercept.IssuerSecretRef.Namespace,
			}
		}
	}
	return out
}

func buildAteletEgressCredentialPolicy(policy *atev1alpha1.EgressCredentialPolicy) *ateletpb.EgressCredentialPolicy {
	if policy == nil {
		return nil
	}
	out := &ateletpb.EgressCredentialPolicy{}
	for _, injection := range policy.Inject {
		outInjection := &ateletpb.EgressCredentialInjection{
			Header: injection.Header,
		}
		if injection.ValueFrom.SecretKeyRef != nil {
			outInjection.ValueFrom = &ateletpb.EgressCredentialValueFrom{
				SecretKeyRef: &ateletpb.SecretKeySelector{
					Name: injection.ValueFrom.SecretKeyRef.Name,
					Key:  injection.ValueFrom.SecretKeyRef.Key,
				},
			}
		}
		out.Inject = append(out.Inject, outInjection)
	}
	return out
}
