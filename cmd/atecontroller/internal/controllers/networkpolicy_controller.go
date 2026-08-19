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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/agent-substrate/substrate/internal/resources"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

const (
	networkPolicyFieldOwner = "ate-networkpolicy"
	ateSystemNamespace      = "ate-system"
	atenetRouterAppName     = "atenet-router"
	atenetEgressAppName     = "atenet-egress"
	dnsAppName              = "dns"
	kubeSystemNamespace     = "kube-system"
	kubeDNSAppName          = "kube-dns"
)

type NetworkPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=ate.dev,resources=workerpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *NetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	wp := &atev1alpha1.WorkerPool{}
	if err := r.Get(ctx, req.NamespacedName, wp); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get worker pool %q: %w", req.NamespacedName, err)
	}

	if !wp.GetDeletionTimestamp().IsZero() {
		log.Info("WorkerPool is being deleted, NetworkPolicy will be GC'd via OwnerReference",
			"namespace", wp.Namespace,
			"name", wp.Name)
		return ctrl.Result{}, nil
	}

	if err := r.reconcileImpl(ctx, wp); err != nil {
		log.Error(err, "Failed to reconcile NetworkPolicy")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *NetworkPolicyReconciler) reconcileImpl(ctx context.Context, wp *atev1alpha1.WorkerPool) error {
	log := log.FromContext(ctx)

	npAC := buildNetworkPolicyApplyConfig(wp)

	if err := r.Apply(ctx, npAC, client.FieldOwner(networkPolicyFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply NetworkPolicy %s:%s: %w", *npAC.Namespace, *npAC.Name, err)
	}
	log.Info("reconcileImpl done",
		"namespace", *npAC.Namespace,
		"name", *npAC.Name)

	return nil
}

func buildNetworkPolicyApplyConfig(wp *atev1alpha1.WorkerPool) *networkingv1ac.NetworkPolicyApplyConfiguration {
	np := networkingv1ac.NetworkPolicy(resources.NetworkPolicyName(wp.Name), wp.Namespace).
		WithLabels(map[string]string{
			"ate.dev/worker-pool": wp.Name,
		}).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(atev1alpha1.GroupVersion.String()).
			WithKind("WorkerPool").
			WithName(wp.Name).
			WithUID(wp.UID).
			WithController(true).
			WithBlockOwnerDeletion(true))

	// Ingress accepts connections only from the atenet-router. Egress is denied
	// except for cluster DNS, Substrate DNS, and the authenticated egress gateway.
	policySpec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector().
			WithMatchLabels(map[string]string{"ate.dev/worker-pool": wp.Name})).
		WithPolicyTypes(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress).
		WithIngress(
			networkingv1ac.NetworkPolicyIngressRule().
				WithFrom(
					networkingv1ac.NetworkPolicyPeer().
						WithNamespaceSelector(metav1ac.LabelSelector().
							WithMatchLabels(map[string]string{"kubernetes.io/metadata.name": ateSystemNamespace})).
						WithPodSelector(metav1ac.LabelSelector().
							WithMatchLabels(map[string]string{"app": atenetRouterAppName})),
				),
		).
		WithEgress(
			dnsEgressRule(kubeSystemNamespace, "k8s-app", kubeDNSAppName),
			dnsEgressRule(ateSystemNamespace, "app", dnsAppName),
			namespacedPodEgressRule(ateSystemNamespace, "app", atenetEgressAppName, 443, corev1.ProtocolTCP),
		)
	np.WithSpec(policySpec)

	return np
}

func dnsEgressRule(namespace, labelKey, labelValue string) *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(namespacedPodPeer(namespace, labelKey, labelValue)).
		WithPorts(
			networkingv1ac.NetworkPolicyPort().WithProtocol(corev1.ProtocolUDP).WithPort(intstr.FromInt32(53)),
			networkingv1ac.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt32(53)),
		)
}

func namespacedPodEgressRule(namespace, labelKey, labelValue string, port int32, protocol corev1.Protocol) *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(namespacedPodPeer(namespace, labelKey, labelValue)).
		WithPorts(networkingv1ac.NetworkPolicyPort().WithProtocol(protocol).WithPort(intstr.FromInt32(port)))
}

func namespacedPodPeer(namespace, labelKey, labelValue string) *networkingv1ac.NetworkPolicyPeerApplyConfiguration {
	return networkingv1ac.NetworkPolicyPeer().
		WithNamespaceSelector(metav1ac.LabelSelector().
			WithMatchLabels(map[string]string{"kubernetes.io/metadata.name": namespace})).
		WithPodSelector(metav1ac.LabelSelector().
			WithMatchLabels(map[string]string{labelKey: labelValue}))
}

func (r *NetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("networkpolicy").
		For(&atev1alpha1.WorkerPool{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
