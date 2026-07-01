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
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/agent-substrate/substrate/internal/ateompath"
)

func TestBuildDeploymentApplyConfig(t *testing.T) {
	requiredNodeAffinity := &ateapipb.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &ateapipb.NodeSelector{
			NodeSelectorTerms: []*ateapipb.NodeSelectorTerm{{
				MatchExpressions: []*ateapipb.NodeSelectorRequirement{{
					Key:      "workload",
					Operator: string(corev1.NodeSelectorOpIn),
					Values:   []string{"substrate"},
				}},
			}},
		},
	}
	preferredNodeAffinity := &ateapipb.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []*ateapipb.PreferredSchedulingTerm{{
			Weight: 50,
			Preference: &ateapipb.NodeSelectorTerm{
				MatchExpressions: []*ateapipb.NodeSelectorRequirement{{
					Key:      "disk",
					Operator: string(corev1.NodeSelectorOpIn),
					Values:   []string{"ssd"},
				}},
			},
		}},
	}
	tolerationSeconds := int64(300)
	toleration := &ateapipb.Toleration{
		Key:               "dedicated",
		Operator:          string(corev1.TolerationOpEqual),
		Value:             "workerpool",
		Effect:            string(corev1.TaintEffectNoSchedule),
		TolerationSeconds: &tolerationSeconds,
	}

	tests := []struct {
		name string
		wp   *ateapipb.WorkerPool
		want *appsv1ac.DeploymentApplyConfiguration
	}{
		{
			name: "default workerpool",
			wp:   testWorkerPoolApplyConfig(nil),
			want: expectedDeploymentApplyConfig(nil),
		},
		{
			name: "with node selector",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				NodeSelector: map[string]string{
					"accelerator": "gpu",
					"topology":    "high-mem",
				},
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.WithNodeSelector(map[string]string{
					"accelerator": "gpu",
					"topology":    "high-mem",
				})
			}),
		},
		{
			name: "with tolerations",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				Tolerations: []*ateapipb.Toleration{toleration},
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.Tolerations = []corev1ac.TolerationApplyConfiguration{
					*corev1ac.Toleration().
						WithKey("dedicated").
						WithOperator(corev1.TolerationOpEqual).
						WithValue("workerpool").
						WithEffect(corev1.TaintEffectNoSchedule).
						WithTolerationSeconds(300),
				}
			}),
		},
		{
			name: "with node affinity",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				NodeAffinity: requiredNodeAffinity,
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.WithAffinity(corev1ac.Affinity().WithNodeAffinity(
					corev1ac.NodeAffinity().WithRequiredDuringSchedulingIgnoredDuringExecution(
						corev1ac.NodeSelector().WithNodeSelectorTerms(
							corev1ac.NodeSelectorTerm().WithMatchExpressions(
								corev1ac.NodeSelectorRequirement().
									WithKey("workload").
									WithOperator(corev1.NodeSelectorOpIn).
									WithValues("substrate"),
							),
						),
					),
				))
			}),
		},
		{
			name: "with priority class name",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				PriorityClassName: "interactive-workerpool",
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.WithPriorityClassName("interactive-workerpool")
			}),
		},
		{
			name: "with resources",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				Resources: &ateapipb.ResourceRequirements{
					Requests: map[string]string{
						string(corev1.ResourceCPU):    "500m",
						string(corev1.ResourceMemory): "1Gi",
					},
					Limits: map[string]string{
						string(corev1.ResourceCPU):    "1",
						string(corev1.ResourceMemory): "2Gi",
					},
				},
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.Containers[0].WithResources(corev1ac.ResourceRequirements().
					WithRequests(corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					}).
					WithLimits(corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					}))
			}),
		},
		{
			name: "with combined scheduling fields",
			wp: testWorkerPoolApplyConfig(&ateapipb.WorkerPoolPodTemplate{
				NodeSelector: map[string]string{
					"accelerator": "gpu",
					"topology":    "high-mem",
				},
				Tolerations:       []*ateapipb.Toleration{toleration},
				PriorityClassName: "interactive-workerpool",
				NodeAffinity:      preferredNodeAffinity,
			}),
			want: expectedDeploymentApplyConfig(func(podSpecAC *corev1ac.PodSpecApplyConfiguration) {
				podSpecAC.WithNodeSelector(map[string]string{
					"accelerator": "gpu",
					"topology":    "high-mem",
				})
				podSpecAC.Tolerations = []corev1ac.TolerationApplyConfiguration{
					*corev1ac.Toleration().
						WithKey("dedicated").
						WithOperator(corev1.TolerationOpEqual).
						WithValue("workerpool").
						WithEffect(corev1.TaintEffectNoSchedule).
						WithTolerationSeconds(300),
				}
				podSpecAC.WithPriorityClassName("interactive-workerpool")
				podSpecAC.WithAffinity(corev1ac.Affinity().WithNodeAffinity(
					corev1ac.NodeAffinity().WithPreferredDuringSchedulingIgnoredDuringExecution(
						corev1ac.PreferredSchedulingTerm().
							WithWeight(50).
							WithPreference(corev1ac.NodeSelectorTerm().WithMatchExpressions(
								corev1ac.NodeSelectorRequirement().
									WithKey("disk").
									WithOperator(corev1.NodeSelectorOpIn).
									WithValues("ssd"),
							)),
					),
				))
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildDeploymentApplyConfig(tt.wp)
			if err != nil {
				t.Fatalf("buildDeploymentApplyConfig() error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("buildDeploymentApplyConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMicroVMPodShape asserts the micro-VM sandbox class adds the /dev/kvm
// device (volume + container mount) and node placement (nodeSelector +
// toleration on ate.dev/sandboxClass); other classes get none of it.
func TestMicroVMPodShape(t *testing.T) {
	tests := []struct {
		name        string
		class       string
		wantMicroVM bool
	}{
		{"gvisor default", "", false},
		{"gvisor explicit", sandboxClassGvisor, false},
		{"microvm", sandboxClassMicroVM, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wp := testWorkerPoolApplyConfig(nil)
			wp.Spec.SandboxClass = tt.class
			dep, err := buildDeploymentApplyConfig(wp)
			if err != nil {
				t.Fatalf("buildDeploymentApplyConfig() error: %v", err)
			}
			ps := dep.Spec.Template.Spec

			hasVol := false
			for _, v := range ps.Volumes {
				if v.Name != nil && *v.Name == "dev-kvm" {
					hasVol = true
					if v.HostPath == nil || v.HostPath.Path == nil || *v.HostPath.Path != "/dev/kvm" ||
						v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathCharDev {
						t.Errorf("dev-kvm volume = %+v, want /dev/kvm CharDevice", v.HostPath)
					}
				}
			}
			hasMount := false
			for _, c := range ps.Containers {
				for _, m := range c.VolumeMounts {
					if m.MountPath != nil && *m.MountPath == "/dev/kvm" {
						hasMount = true
					}
				}
			}
			_, hasSelector := ps.NodeSelector["ate.dev/sandboxClass"]
			hasTol := false
			for _, tol := range ps.Tolerations {
				if tol.Key != nil && *tol.Key == "ate.dev/sandboxClass" {
					hasTol = true
				}
			}
			if hasVol != tt.wantMicroVM || hasMount != tt.wantMicroVM || hasSelector != tt.wantMicroVM || hasTol != tt.wantMicroVM {
				t.Errorf("microvm shape: vol=%v mount=%v selector=%v toleration=%v, want all %v",
					hasVol, hasMount, hasSelector, hasTol, tt.wantMicroVM)
			}
		})
	}
}

func testWorkerPoolApplyConfig(tmpl *ateapipb.WorkerPoolPodTemplate) *ateapipb.WorkerPool {
	return &ateapipb.WorkerPool{
		Name: "pool",
		Spec: &ateapipb.WorkerPoolSpec{
			Replicas:           2,
			AteomImage:         "ateom:v1",
			Template:           tmpl,
			DeploymentAtespace: "default",
		},
	}
}

func expectedDeploymentApplyConfig(mutatePodSpec func(*corev1ac.PodSpecApplyConfiguration)) *appsv1ac.DeploymentApplyConfiguration {
	wp := testWorkerPoolApplyConfig(nil)

	podSpecAC := corev1ac.PodSpec().
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsUser(0).
			WithRunAsGroup(0)).
		WithVolumes(corev1ac.Volume().
			WithName("run-ateom").
			WithHostPath(corev1ac.HostPathVolumeSource().
				WithPath(ateompath.BasePath).
				WithType(corev1.HostPathDirectoryOrCreate))).
		WithContainers(corev1ac.Container().
			WithName("ateom").
			WithImage(wp.GetSpec().GetAteomImage()).
			WithArgs("--pod-uid=$(POD_UID)").
			WithSecurityContext(corev1ac.SecurityContext().
				WithPrivileged(true).
				WithRunAsUser(0).
				WithRunAsGroup(0)).
			WithEnv(corev1ac.EnvVar().
				WithName("POD_UID").
				WithValueFrom(corev1ac.EnvVarSource().
					WithFieldRef(corev1ac.ObjectFieldSelector().
						WithFieldPath("metadata.uid")))).
			WithVolumeMounts(corev1ac.VolumeMount().
				WithName("run-ateom").
				WithMountPath(ateompath.BasePath)).
			WithResources(corev1ac.ResourceRequirements()))

	podSpecAC.NodeSelector = map[string]string{}
	podSpecAC.Tolerations = []corev1ac.TolerationApplyConfiguration{}
	podSpecAC.WithPriorityClassName("")
	podSpecAC.WithAffinity(corev1ac.Affinity())
	if mutatePodSpec != nil {
		mutatePodSpec(podSpecAC)
	}

	return appsv1ac.Deployment(deploymentName(wp.GetName()), wp.GetSpec().GetDeploymentAtespace()).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(wp.GetSpec().GetReplicas()).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"ate.dev/worker-pool": wp.GetName()})).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(map[string]string{"ate.dev/worker-pool": wp.GetName()}).
				WithSpec(podSpecAC)))
}
