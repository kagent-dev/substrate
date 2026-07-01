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
	"fmt"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
)

const (
	workerPoolFieldOwner = "workerpool-controller"
	sandboxClassGvisor   = "gvisor"
	sandboxClassMicroVM  = "microvm"
)

// buildDeploymentApplyConfig constructs the SSA apply configuration for the
// Deployment managed by a WorkerPool. Only fields owned by this controller
// are declared here.
func buildDeploymentApplyConfig(wp *ateapipb.WorkerPool) (*appsv1ac.DeploymentApplyConfiguration, error) {
	containerAC := corev1ac.Container().
		WithName("ateom").
		WithImage(wp.GetSpec().GetAteomImage()).
		WithArgs(
			"--pod-uid=$(POD_UID)",
		).
		WithSecurityContext(corev1ac.SecurityContext().
			WithPrivileged(true).
			WithRunAsUser(0).
			WithRunAsGroup(0)).
		WithEnv(
			corev1ac.EnvVar().
				WithName("POD_UID").
				WithValueFrom(corev1ac.EnvVarSource().
					WithFieldRef(corev1ac.ObjectFieldSelector().
						WithFieldPath("metadata.uid"))),
		).
		WithVolumeMounts(corev1ac.VolumeMount().
			WithName("run-ateom").
			WithMountPath(ateompath.BasePath))

	podSpecAC := corev1ac.PodSpec().
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsUser(0).
			WithRunAsGroup(0)).
		WithVolumes(corev1ac.Volume().
			WithName("run-ateom").
			WithHostPath(corev1ac.HostPathVolumeSource().
				WithPath(ateompath.BasePath).
				WithType(corev1.HostPathDirectoryOrCreate)))

	if err := applyWorkerPoolPodTemplate(podSpecAC, containerAC, wp.GetSpec().GetTemplate()); err != nil {
		return nil, err
	}
	maybeApplyMicroVMPodShape(podSpecAC, containerAC, wp.GetSpec().GetSandboxClass())
	podSpecAC.WithContainers(containerAC)

	return appsv1ac.Deployment(deploymentName(wp.GetName()), wp.GetSpec().GetDeploymentAtespace()).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(wp.GetSpec().GetReplicas()).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"ate.dev/worker-pool": wp.GetName()})).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(map[string]string{
					"ate.dev/worker-pool": wp.GetName(),
				}).
				WithSpec(podSpecAC))), nil
}

// maybeApplyMicroVMPodShape adds the /dev/kvm device and node placement a
// micro-VM (kata + cloud-hypervisor) worker pool needs, on top of any
// pod-template settings. No-op unless sandboxClass is the micro-VM class.
//
// TODO: this hardcodes one sandbox class's pod requirements in the controller.
// Consider making it generic so a sandbox class can declare its own pod shape
// (e.g. required devices/mounts + node placement on the SandboxConfig spec)
// instead of branching on SandboxClass here, so new classes don't need a
// controller change.
func maybeApplyMicroVMPodShape(
	podSpecAC *corev1ac.PodSpecApplyConfiguration,
	containerAC *corev1ac.ContainerApplyConfiguration,
	sandboxClass string,
) {
	if sandboxClass != sandboxClassMicroVM {
		return
	}

	// The micro-VM runtime needs /dev/kvm. The container is already privileged
	// (so it can also reach vhost devices), but we mount /dev/kvm explicitly.
	containerAC.WithVolumeMounts(corev1ac.VolumeMount().
		WithName("dev-kvm").
		WithMountPath("/dev/kvm"))
	podSpecAC.WithVolumes(corev1ac.Volume().
		WithName("dev-kvm").
		WithHostPath(corev1ac.HostPathVolumeSource().
			WithPath("/dev/kvm").
			WithType(corev1.HostPathCharDev)))

	// Pin placement to KVM-capable, nested-virt nodes via nodeSelector +
	// toleration on ate.dev/sandboxClass=microvm. This is our own convention
	// (GKE attaches no label/taint to nested-virt pools): applied to kind nodes
	// by hack/create-kind-cluster.sh and via --node-labels at GKE pool creation.
	// Additive on top of the WorkerPool's configurable scheduling fields
	// (spec.template nodeSelector/tolerations/affinity, added in #247) — merge,
	// don't overwrite.
	if podSpecAC.NodeSelector == nil {
		podSpecAC.NodeSelector = map[string]string{}
	}
	podSpecAC.NodeSelector["ate.dev/sandboxClass"] = sandboxClassMicroVM
	podSpecAC.WithTolerations(corev1ac.Toleration().
		WithKey("ate.dev/sandboxClass").
		WithOperator(corev1.TolerationOpEqual).
		WithValue(sandboxClassMicroVM).
		WithEffect(corev1.TaintEffectNoSchedule))
}

func applyWorkerPoolPodTemplate(
	podSpecAC *corev1ac.PodSpecApplyConfiguration,
	containerAC *corev1ac.ContainerApplyConfiguration,
	tmpl *ateapipb.WorkerPoolPodTemplate,
) error {
	podSpecAC.NodeSelector = map[string]string{}
	podSpecAC.Tolerations = []corev1ac.TolerationApplyConfiguration{}
	podSpecAC.WithPriorityClassName("")
	podSpecAC.WithAffinity(corev1ac.Affinity())
	resourcesAC := corev1ac.ResourceRequirements()
	containerAC.WithResources(resourcesAC)

	if tmpl == nil {
		return nil
	}

	if tmpl.NodeSelector != nil {
		podSpecAC.WithNodeSelector(tmpl.GetNodeSelector())
	}
	podSpecAC.Tolerations = tolerationApplyValues(tolerationsToApply(tmpl.GetTolerations()))
	podSpecAC.WithPriorityClassName(tmpl.GetPriorityClassName())

	if tmpl.GetNodeAffinity() != nil {
		podSpecAC.WithAffinity(corev1ac.Affinity().WithNodeAffinity(nodeAffinityToApply(tmpl.GetNodeAffinity())))
	}

	if tmpl.GetResources() != nil {
		requests, err := resourceList(tmpl.GetResources().GetRequests())
		if err != nil {
			return err
		}
		limits, err := resourceList(tmpl.GetResources().GetLimits())
		if err != nil {
			return err
		}
		if requests != nil {
			resourcesAC.WithRequests(requests)
		}
		if limits != nil {
			resourcesAC.WithLimits(limits)
		}
	}
	return nil
}

func tolerationApplyValues(tolerations []*corev1ac.TolerationApplyConfiguration) []corev1ac.TolerationApplyConfiguration {
	out := make([]corev1ac.TolerationApplyConfiguration, 0, len(tolerations))
	for _, toleration := range tolerations {
		out = append(out, *toleration)
	}
	return out
}

func tolerationsToApply(tolerations []*ateapipb.Toleration) []*corev1ac.TolerationApplyConfiguration {
	out := make([]*corev1ac.TolerationApplyConfiguration, 0, len(tolerations))
	for i := range tolerations {
		t := tolerations[i]
		ac := corev1ac.Toleration()
		if t.GetKey() != "" {
			ac.WithKey(t.GetKey())
		}
		if t.GetOperator() != "" {
			ac.WithOperator(corev1.TolerationOperator(t.GetOperator()))
		}
		if t.GetValue() != "" {
			ac.WithValue(t.GetValue())
		}
		if t.GetEffect() != "" {
			ac.WithEffect(corev1.TaintEffect(t.GetEffect()))
		}
		if t.TolerationSeconds != nil {
			ac.WithTolerationSeconds(t.GetTolerationSeconds())
		}
		out = append(out, ac)
	}
	return out
}

func nodeAffinityToApply(na *ateapipb.NodeAffinity) *corev1ac.NodeAffinityApplyConfiguration {
	ac := corev1ac.NodeAffinity()
	if na.GetRequiredDuringSchedulingIgnoredDuringExecution() != nil {
		ac.WithRequiredDuringSchedulingIgnoredDuringExecution(nodeSelectorToApply(na.GetRequiredDuringSchedulingIgnoredDuringExecution()))
	}
	for i := range na.GetPreferredDuringSchedulingIgnoredDuringExecution() {
		term := na.GetPreferredDuringSchedulingIgnoredDuringExecution()[i]
		ac.WithPreferredDuringSchedulingIgnoredDuringExecution(preferredSchedulingTermToApply(term))
	}
	return ac
}

func nodeSelectorToApply(ns *ateapipb.NodeSelector) *corev1ac.NodeSelectorApplyConfiguration {
	ac := corev1ac.NodeSelector()
	for i := range ns.GetNodeSelectorTerms() {
		ac.WithNodeSelectorTerms(nodeSelectorTermToApply(ns.GetNodeSelectorTerms()[i]))
	}
	return ac
}

func preferredSchedulingTermToApply(term *ateapipb.PreferredSchedulingTerm) *corev1ac.PreferredSchedulingTermApplyConfiguration {
	return corev1ac.PreferredSchedulingTerm().
		WithWeight(term.GetWeight()).
		WithPreference(nodeSelectorTermToApply(term.GetPreference()))
}

func nodeSelectorTermToApply(term *ateapipb.NodeSelectorTerm) *corev1ac.NodeSelectorTermApplyConfiguration {
	ac := corev1ac.NodeSelectorTerm()
	for i := range term.GetMatchExpressions() {
		ac.WithMatchExpressions(nodeSelectorRequirementToApply(term.GetMatchExpressions()[i]))
	}
	for i := range term.GetMatchFields() {
		ac.WithMatchFields(nodeSelectorRequirementToApply(term.GetMatchFields()[i]))
	}
	return ac
}

func nodeSelectorRequirementToApply(req *ateapipb.NodeSelectorRequirement) *corev1ac.NodeSelectorRequirementApplyConfiguration {
	ac := corev1ac.NodeSelectorRequirement().WithKey(req.GetKey()).WithOperator(corev1.NodeSelectorOperator(req.GetOperator()))
	if len(req.GetValues()) > 0 {
		ac.WithValues(req.GetValues()...)
	}
	return ac
}

func resourceList(in map[string]string) (corev1.ResourceList, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := corev1.ResourceList{}
	for k, v := range in {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("invalid resource quantity %s=%q: %w", k, v, err)
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, nil
}

func deploymentName(wpName string) string {
	return wpName + "-deployment"
}
