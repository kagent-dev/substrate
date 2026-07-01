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

package controllers

import (
	"context"
	"fmt"
	"time"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type APIWorkerPoolProjector struct {
	client.Client
	AteClient ateapipb.ControlClient
}

func (p *APIWorkerPoolProjector) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("api-workerpool-projector")
	for {
		stream, err := p.AteClient.WatchWorkerPools(ctx, &ateapipb.WatchWorkerPoolsRequest{})
		if err != nil {
			log.Error(err, "watch WorkerPools failed")
			if !sleepOrDone(ctx, time.Second) {
				return nil
			}
			continue
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				log.Error(err, "WorkerPool watch stream closed")
				break
			}
			if err := p.applyEvent(ctx, event); err != nil {
				log.Error(err, "project WorkerPool", "namespace", event.GetWorkerPool().GetNamespace(), "name", event.GetWorkerPool().GetName())
			}
		}
		if !sleepOrDone(ctx, time.Second) {
			return nil
		}
	}
}

func (p *APIWorkerPoolProjector) applyEvent(ctx context.Context, event *ateapipb.WatchWorkerPoolsResponse) error {
	wp := event.GetWorkerPool()
	if wp.GetNamespace() == "" || wp.GetName() == "" {
		return nil
	}
	if event.GetType() == ateapipb.ResourceEventType_RESOURCE_EVENT_TYPE_DELETED {
		return client.IgnoreNotFound(p.Delete(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: wp.GetNamespace(),
				Name:      deploymentName(wp.GetName()),
			},
		}))
	}
	apiWP, err := protoWorkerPoolToAPI(wp)
	if err != nil {
		return err
	}
	depAC := buildDeploymentApplyConfig(apiWP)
	depAC.OwnerReferences = nil
	if err := p.Apply(ctx, depAC, client.FieldOwner(workerPoolFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply Deployment: %w", err)
	}
	return nil
}

func protoWorkerPoolToAPI(in *ateapipb.WorkerPool) (*atev1alpha1.WorkerPool, error) {
	spec, err := protoWorkerPoolSpecToAPI(in.GetSpec())
	if err != nil {
		return nil, err
	}
	return &atev1alpha1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: in.GetNamespace(),
			Name:      in.GetName(),
			Labels:    copyStringMap(in.GetLabels()),
		},
		Spec: spec,
	}, nil
}

func protoWorkerPoolSpecToAPI(in *ateapipb.WorkerPoolSpec) (atev1alpha1.WorkerPoolSpec, error) {
	tmpl, err := protoWorkerPoolPodTemplateToAPI(in.GetTemplate())
	if err != nil {
		return atev1alpha1.WorkerPoolSpec{}, err
	}
	return atev1alpha1.WorkerPoolSpec{
		Replicas:          in.GetReplicas(),
		AteomImage:        in.GetAteomImage(),
		Template:          tmpl,
		SandboxClass:      atev1alpha1.SandboxClass(in.GetSandboxClass()),
		SandboxConfigName: in.GetSandboxConfigName(),
	}, nil
}

func protoWorkerPoolPodTemplateToAPI(in *ateapipb.WorkerPoolPodTemplate) (*atev1alpha1.WorkerPoolPodTemplate, error) {
	if in == nil {
		return nil, nil
	}
	resources, err := protoResourceRequirementsToAPI(in.GetResources())
	if err != nil {
		return nil, err
	}
	return &atev1alpha1.WorkerPoolPodTemplate{
		NodeSelector:      copyStringMap(in.GetNodeSelector()),
		Tolerations:       protoTolerationsToAPI(in.GetTolerations()),
		PriorityClassName: in.GetPriorityClassName(),
		NodeAffinity:      protoNodeAffinityToAPI(in.GetNodeAffinity()),
		Resources:         resources,
	}, nil
}

func protoTolerationsToAPI(in []*ateapipb.Toleration) []corev1.Toleration {
	out := make([]corev1.Toleration, 0, len(in))
	for _, t := range in {
		var seconds *int64
		if t.TolerationSeconds != nil {
			seconds = ptrVal(t.GetTolerationSeconds())
		}
		out = append(out, corev1.Toleration{Key: t.GetKey(), Operator: corev1.TolerationOperator(t.GetOperator()), Value: t.GetValue(), Effect: corev1.TaintEffect(t.GetEffect()), TolerationSeconds: seconds})
	}
	return out
}

func protoNodeAffinityToAPI(in *ateapipb.NodeAffinity) *corev1.NodeAffinity {
	if in == nil {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution:  protoNodeSelectorToAPI(in.GetRequiredDuringSchedulingIgnoredDuringExecution()),
		PreferredDuringSchedulingIgnoredDuringExecution: protoPreferredSchedulingTermsToAPI(in.GetPreferredDuringSchedulingIgnoredDuringExecution()),
	}
}

func protoNodeSelectorToAPI(in *ateapipb.NodeSelector) *corev1.NodeSelector {
	if in == nil {
		return nil
	}
	out := &corev1.NodeSelector{}
	for _, term := range in.GetNodeSelectorTerms() {
		out.NodeSelectorTerms = append(out.NodeSelectorTerms, protoNodeSelectorTermToAPI(term))
	}
	return out
}

func protoPreferredSchedulingTermsToAPI(in []*ateapipb.PreferredSchedulingTerm) []corev1.PreferredSchedulingTerm {
	out := make([]corev1.PreferredSchedulingTerm, 0, len(in))
	for _, term := range in {
		out = append(out, corev1.PreferredSchedulingTerm{Weight: term.GetWeight(), Preference: protoNodeSelectorTermToAPI(term.GetPreference())})
	}
	return out
}

func protoNodeSelectorTermToAPI(in *ateapipb.NodeSelectorTerm) corev1.NodeSelectorTerm {
	if in == nil {
		return corev1.NodeSelectorTerm{}
	}
	return corev1.NodeSelectorTerm{
		MatchExpressions: protoNodeSelectorRequirementsToAPI(in.GetMatchExpressions()),
		MatchFields:      protoNodeSelectorRequirementsToAPI(in.GetMatchFields()),
	}
}

func protoNodeSelectorRequirementsToAPI(in []*ateapipb.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	out := make([]corev1.NodeSelectorRequirement, 0, len(in))
	for _, req := range in {
		out = append(out, corev1.NodeSelectorRequirement{Key: req.GetKey(), Operator: corev1.NodeSelectorOperator(req.GetOperator()), Values: append([]string(nil), req.GetValues()...)})
	}
	return out
}

func protoResourceRequirementsToAPI(in *ateapipb.ResourceRequirements) (*corev1.ResourceRequirements, error) {
	if in == nil {
		return nil, nil
	}
	limits, err := protoResourceListToAPI(in.GetLimits())
	if err != nil {
		return nil, err
	}
	requests, err := protoResourceListToAPI(in.GetRequests())
	if err != nil {
		return nil, err
	}
	return &corev1.ResourceRequirements{Limits: limits, Requests: requests}, nil
}

func protoResourceListToAPI(in map[string]string) (corev1.ResourceList, error) {
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

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ptrVal[T any](v T) *T { return &v }

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
