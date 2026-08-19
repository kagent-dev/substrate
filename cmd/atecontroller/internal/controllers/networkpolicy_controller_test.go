// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/agent-substrate/substrate/internal/resources"
)

func TestWorkerPoolCreatesNetworkPolicy(t *testing.T) {
	ctx := t.Context()
	wp := makeWorkerPool("test-netpolicy-create", "default", 2, "ateom:v1")
	if err := k8sClient.Create(ctx, wp); err != nil {
		t.Fatalf("create WorkerPool: %v", err)
	}
	deleteOnCleanup(t, wp)

	eventually(t, func(ctx context.Context) (bool, error) {
		npName := resources.NetworkPolicyName(wp.Name)
		np := &networkingv1.NetworkPolicy{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: npName, Namespace: wp.Namespace}, np)
		if err != nil {
			return false, nil
		}

		// Verify OwnerReference
		if len(np.OwnerReferences) == 0 || np.OwnerReferences[0].Name != wp.Name {
			return false, nil
		}

		// Verify metadata label matches the worker pool
		if np.Labels == nil || np.Labels["ate.dev/worker-pool"] != wp.Name {
			return false, nil
		}

		// Verify PodSelector matches the worker pool
		if np.Spec.PodSelector.MatchLabels == nil || np.Spec.PodSelector.MatchLabels["ate.dev/worker-pool"] != wp.Name {
			return false, nil
		}

		// Verify PolicyTypes contains Ingress and Egress.
		hasIngress := false
		hasEgress := false
		for _, pt := range np.Spec.PolicyTypes {
			if pt == networkingv1.PolicyTypeIngress {
				hasIngress = true
			}
			if pt == networkingv1.PolicyTypeEgress {
				hasEgress = true
			}
		}
		if !hasIngress || !hasEgress {
			return false, nil
		}

		// Verify Ingress Rules (Allow only ingress from ATE router)
		if len(np.Spec.Ingress) != 1 {
			return false, nil
		}
		ingressRule := np.Spec.Ingress[0]
		if len(ingressRule.From) != 1 {
			return false, nil
		}
		fromPeer := ingressRule.From[0]
		if fromPeer.NamespaceSelector == nil || fromPeer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != ateSystemNamespace {
			return false, nil
		}
		if fromPeer.PodSelector == nil || fromPeer.PodSelector.MatchLabels["app"] != atenetRouterAppName {
			return false, nil
		}

		// Cluster DNS, Substrate DNS, and atenet-egress are the only egress rules.
		if len(np.Spec.Egress) != 3 {
			return false, nil
		}

		return true, nil
	})
}

func TestBuildNetworkPolicyRestrictsWorkerEgress(t *testing.T) {
	wp := makeWorkerPool("test-netpolicy-egress", "default", 1, "ateom:v1")
	spec := buildNetworkPolicyApplyConfig(wp).Spec

	hasEgress := false
	for _, policyType := range spec.PolicyTypes {
		if policyType == networkingv1.PolicyTypeEgress {
			hasEgress = true
		}
	}
	if !hasEgress {
		t.Fatal("PolicyTypes does not contain Egress")
	}

	want := []struct {
		name      string
		namespace string
		labelKey  string
		labelVal  string
		port      int
		protocol  corev1.Protocol
	}{
		{name: "cluster DNS UDP", namespace: "kube-system", labelKey: "k8s-app", labelVal: "kube-dns", port: 53, protocol: corev1.ProtocolUDP},
		{name: "cluster DNS TCP", namespace: "kube-system", labelKey: "k8s-app", labelVal: "kube-dns", port: 53, protocol: corev1.ProtocolTCP},
		{name: "Substrate DNS UDP", namespace: "ate-system", labelKey: "app", labelVal: "dns", port: 53, protocol: corev1.ProtocolUDP},
		{name: "Substrate DNS TCP", namespace: "ate-system", labelKey: "app", labelVal: "dns", port: 53, protocol: corev1.ProtocolTCP},
		{name: "authenticated egress gateway", namespace: "ate-system", labelKey: "app", labelVal: "atenet-egress", port: 443, protocol: corev1.ProtocolTCP},
	}

	for _, expectation := range want {
		t.Run(expectation.name, func(t *testing.T) {
			if !hasEgressDestination(spec.Egress, expectation.namespace, expectation.labelKey, expectation.labelVal, expectation.port, expectation.protocol) {
				t.Errorf("missing egress destination namespace=%q %s=%q port=%d/%s", expectation.namespace, expectation.labelKey, expectation.labelVal, expectation.port, expectation.protocol)
			}
		})
	}
}

func hasEgressDestination(rules []networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration, namespace, labelKey, labelVal string, port int, protocol corev1.Protocol) bool {
	for _, rule := range rules {
		for _, peer := range rule.To {
			if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != namespace {
				continue
			}
			if peer.PodSelector == nil || peer.PodSelector.MatchLabels[labelKey] != labelVal {
				continue
			}
			for _, allowedPort := range rule.Ports {
				if allowedPort.Port != nil && allowedPort.Port.IntValue() == port && allowedPort.Protocol != nil && *allowedPort.Protocol == protocol {
					return true
				}
			}
		}
	}
	return false
}
