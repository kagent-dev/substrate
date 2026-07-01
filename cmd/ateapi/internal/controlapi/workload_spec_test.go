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

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestWorkloadSpecFromActorTemplate(t *testing.T) {
	got := workloadSpecFromActorTemplate(&ateapipb.ActorTemplate{
		Atespace: "agent-ns",
		Name:     "tmpl1",
		Spec: &ateapipb.ActorTemplateSpec{
			PauseImage: "pause",
			Volumes: []*ateapipb.Volume{
				{Name: "skip"},
				{Name: "home", VolumeSource: &ateapipb.VolumeSource{DurableDir: &ateapipb.DurableDirVolumeSource{}}},
			},
			Containers: []*ateapipb.Container{
				{
					Name:    "main",
					Image:   "main",
					Command: []string{"/main"},
					Env: []*ateapipb.EnvVar{
						{Name: "IGNORED", Value: ptr.To("plain")},
					},
					Readyz: &ateapipb.ContainerReadyz{
						HttpGet: &ateapipb.HTTPGetAction{Path: "/health", Port: 8080},
					},
					VolumeMounts: []*ateapipb.VolumeMount{
						{Name: "home", MountPath: "/workspace"},
					},
				},
			},
		},
	})

	want := &ateletpb.WorkloadSpec{
		PauseImage: "pause",
		Volumes: []*ateletpb.Volume{
			{
				Name:   "home",
				Type:   ateletpb.VolumeType_VOLUME_TYPE_DURABLE_DIR,
				Source: &ateletpb.Volume_DurableDir{DurableDir: &ateletpb.DurableDirVolume{}},
			},
		},
		Containers: []*ateletpb.Container{
			{
				Name:    "main",
				Image:   "main",
				Command: []string{"/main"},
				Readyz: &ateletpb.Readyz{
					HttpGet: &ateletpb.HTTPGetAction{Path: "/health", Port: 8080},
				},
				VolumeMounts: []*ateletpb.VolumeMount{
					{Name: "home", MountPath: "/workspace"},
				},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
	}
}

func TestWorkloadSpecFromActorTemplateWithEnv(t *testing.T) {
	tests := []struct {
		name        string
		secrets     []runtime.Object
		template    *ateapipb.ActorTemplate
		want        *ateletpb.WorkloadSpec
		wantErrCode codes.Code
	}{
		{
			name: "resolves literal and secretKeyRef env",
			secrets: []runtime.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "some-secret", Namespace: "agent-ns"},
					Data:       map[string][]byte{"some-key": []byte("some-value")},
				},
			},
			template: envTemplate(
				&ateapipb.EnvVar{Name: "LITERAL", Value: ptr.To("plain")},
				&ateapipb.EnvVar{
					Name: "SOME_KEY",
					ValueFrom: &ateapipb.EnvVarSource{
						SecretKeyRef: &ateapipb.SecretKeySelector{Name: "some-secret", Key: "some-key"},
					},
				},
			),
			want: &ateletpb.WorkloadSpec{
				PauseImage: "pause",
				Containers: []*ateletpb.Container{
					{
						Name:    "main",
						Image:   "main",
						Command: []string{"/main"},
						Env: []*ateletpb.EnvEntry{
							{Name: "LITERAL", Value: "plain"},
							{Name: "SOME_KEY", Value: "some-value"},
						},
					},
				},
			},
		},
		{
			name: "skips optional missing secret",
			template: envTemplate(&ateapipb.EnvVar{
				Name: "OPTIONAL",
				ValueFrom: &ateapipb.EnvVarSource{
					SecretKeyRef: &ateapipb.SecretKeySelector{Name: "missing", Key: "key", Optional: ptr.To(true)},
				},
			}),
			want: &ateletpb.WorkloadSpec{
				PauseImage: "pause",
				Containers: []*ateletpb.Container{{Name: "main", Image: "main", Command: []string{"/main"}}},
			},
		},
		{
			name: "required missing secret fails",
			template: envTemplate(&ateapipb.EnvVar{
				Name: "REQUIRED",
				ValueFrom: &ateapipb.EnvVarSource{
					SecretKeyRef: &ateapipb.SecretKeySelector{Name: "missing", Key: "key"},
				},
			}),
			wantErrCode: codes.FailedPrecondition,
		},
		{
			name: "empty valueFrom fails",
			template: envTemplate(&ateapipb.EnvVar{
				Name:      "EMPTY",
				ValueFrom: &ateapipb.EnvVarSource{},
			}),
			wantErrCode: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := workloadSpecFromActorTemplateWithEnv(context.Background(), fake.NewSimpleClientset(tt.secrets...), nil, tt.template)
			if tt.wantErrCode != codes.OK {
				if status.Code(err) != tt.wantErrCode {
					t.Fatalf("error code = %v, want %v: %v", status.Code(err), tt.wantErrCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("workloadSpecFromActorTemplateWithEnv failed: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWorkloadSpecFromActorTemplateWithEnvCachesSecretsAcrossCalls(t *testing.T) {
	ctx := context.Background()
	secretCache := newEnvSecretCache(envSecretCacheTTL)
	kubeClient := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "some-secret", Namespace: "agent-ns"},
			Data:       map[string][]byte{"some-key": []byte("some-value")},
		},
	)
	actorTemplate := envTemplate(&ateapipb.EnvVar{
		Name: "SOME_KEY",
		ValueFrom: &ateapipb.EnvVarSource{
			SecretKeyRef: &ateapipb.SecretKeySelector{Name: "some-secret", Key: "some-key"},
		},
	})

	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate); err != nil {
		t.Fatalf("first workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate); err != nil {
		t.Fatalf("second workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if got := secretGetCount(kubeClient); got != 1 {
		t.Fatalf("secret gets before TTL expiry = %d, want 1", got)
	}

	expireSecretCache(secretCache)
	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate); err != nil {
		t.Fatalf("third workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if got := secretGetCount(kubeClient); got != 2 {
		t.Fatalf("secret gets after TTL expiry = %d, want 2", got)
	}
}

func envTemplate(env ...*ateapipb.EnvVar) *ateapipb.ActorTemplate {
	return &ateapipb.ActorTemplate{
		Atespace: "agent-ns",
		Name:     "tmpl1",
		Spec: &ateapipb.ActorTemplateSpec{
			PauseImage: "pause",
			Containers: []*ateapipb.Container{
				{Name: "main", Image: "main", Command: []string{"/main"}, Env: env},
			},
		},
	}
}

func expireSecretCache(secretCache *envSecretCache) {
	secretCache.mu.Lock()
	defer secretCache.mu.Unlock()

	for key, entry := range secretCache.entries {
		entry.expiresAt = entry.expiresAt.Add(-envSecretCacheTTL)
		secretCache.entries[key] = entry
	}
}

func secretGetCount(kubeClient *fake.Clientset) int {
	count := 0
	for _, action := range kubeClient.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			count++
		}
	}
	return count
}
