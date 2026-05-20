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
	"context"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/api/v1alpha1"
	"github.com/agent-substrate/substrate/proto/ateletpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorkloadSpecFromActorTemplateResolvesValueFromEnv(t *testing.T) {
	ctx := context.Background()
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "settings",
				Namespace: "agent-ns",
			},
			Data: map[string]string{
				"interval": "45",
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-keys",
				Namespace: "agent-ns",
			},
			Data: map[string][]byte{
				"anthropic": []byte("sk-test"),
			},
		},
	)

	got, err := workloadSpecFromActorTemplate(ctx, kubeClient, &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			PauseImage: "pause",
			Containers: []atev1alpha1.Container{
				{
					Name:    "main",
					Image:   "main",
					Command: []string{"/main"},
					Env: []corev1.EnvVar{
						{
							Name:  "LITERAL",
							Value: "plain",
						},
						{
							Name: "INTERVAL_SECONDS",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
									Key:                  "interval",
								},
							},
						},
						{
							Name: "ANTHROPIC_API_KEY",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "api-keys"},
									Key:                  "anthropic",
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("workloadSpecFromActorTemplate failed: %v", err)
	}

	want := &ateletpb.WorkloadSpec{
		PauseImage: "pause",
		Containers: []*ateletpb.Container{
			{
				Name:    "main",
				Image:   "main",
				Command: []string{"/main"},
				Env: []*ateletpb.EnvEntry{
					{Name: "LITERAL", Value: "plain"},
					{Name: "INTERVAL_SECONDS", Value: "45"},
					{Name: "ANTHROPIC_API_KEY", Value: "sk-test"},
				},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
	}
}

func TestWorkloadSpecFromActorTemplateOptionalConfigMapKeyRefSkipsMissingKey(t *testing.T) {
	optional := true
	got, err := workloadSpecFromActorTemplate(context.Background(), fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "settings",
			Namespace: "agent-ns",
		},
		Data: map[string]string{},
	}), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []corev1.EnvVar{
						{
							Name: "OPTIONAL",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
									Key:                  "missing",
									Optional:             &optional,
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("workloadSpecFromActorTemplate failed: %v", err)
	}
	if len(got.GetContainers()) != 1 {
		t.Fatalf("expected one container, got %d", len(got.GetContainers()))
	}
	if len(got.GetContainers()[0].GetEnv()) != 0 {
		t.Fatalf("expected optional missing env to be skipped, got %v", got.GetContainers()[0].GetEnv())
	}
}

func TestWorkloadSpecFromActorTemplateConfigMapKeyRefMissingConfigMapFails(t *testing.T) {
	_, err := workloadSpecFromActorTemplate(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []corev1.EnvVar{
						{
							Name: "REQUIRED",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
									Key:                  "key",
								},
							},
						},
					},
				},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}

func TestWorkloadSpecFromActorTemplateOptionalSecretKeyRefSkipsMissingSecret(t *testing.T) {
	optional := true
	got, err := workloadSpecFromActorTemplate(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []corev1.EnvVar{
						{
							Name: "OPTIONAL",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
									Key:                  "key",
									Optional:             &optional,
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("workloadSpecFromActorTemplate failed: %v", err)
	}
	if len(got.GetContainers()) != 1 {
		t.Fatalf("expected one container, got %d", len(got.GetContainers()))
	}
	if len(got.GetContainers()[0].GetEnv()) != 0 {
		t.Fatalf("expected optional missing env to be skipped, got %v", got.GetContainers()[0].GetEnv())
	}
}

func TestWorkloadSpecFromActorTemplateSecretKeyRefMissingSecretFails(t *testing.T) {
	_, err := workloadSpecFromActorTemplate(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []corev1.EnvVar{
						{
							Name: "REQUIRED",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
									Key:                  "key",
								},
							},
						},
					},
				},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}

func TestWorkloadSpecFromActorTemplateUnsupportedValueFromFails(t *testing.T) {
	_, err := workloadSpecFromActorTemplate(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []corev1.EnvVar{
						{
							Name: "FIELD",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							},
						},
					},
				},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}
