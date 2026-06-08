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
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveVolumesConfigMap(t *testing.T) {
	ctx := context.Background()
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "skills", Namespace: "agent-ns"},
			Data: map[string]string{
				"SKILL.md": "# hello",
			},
		},
	)

	got, err := resolveVolumes(ctx, kubeClient, &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Volumes: []corev1.Volume{
				{
					Name: "skills",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "skills"},
						},
					},
				},
			},
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "skills", MountPath: "/sandbox/skills", ReadOnly: true},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveVolumes failed: %v", err)
	}

	want := []*ateletpb.ResolvedVolume{
		{
			Name: "skills",
			Files: []*ateletpb.VolumeFile{
				{RelativePath: "SKILL.md", Content: []byte("# hello"), Mode: defaultConfigMapFileMode},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Fatalf("ResolvedVolume mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveVolumesRejectsUnsupportedSource(t *testing.T) {
	_, err := resolveVolumes(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Volumes: []corev1.Volume{
				{
					Name: "bad",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"},
					},
				},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}

func TestResolveVolumesRejectsUndefinedMount(t *testing.T) {
	_, err := resolveVolumes(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "missing", MountPath: "/data"},
					},
				},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}

func TestResolveVolumesOptionalMissingConfigMap(t *testing.T) {
	optional := true
	got, err := resolveVolumes(context.Background(), fake.NewSimpleClientset(), &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Volumes: []corev1.Volume{
				{
					Name: "optional-cm",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
							Optional:             &optional,
						},
					},
				},
			},
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "optional-cm", MountPath: "/data"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveVolumes failed: %v", err)
	}
	want := []*ateletpb.ResolvedVolume{{Name: "optional-cm"}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Fatalf("ResolvedVolume mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveVolumesConfigMapBinaryData(t *testing.T) {
	ctx := context.Background()
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "bin", Namespace: "agent-ns"},
			BinaryData: map[string][]byte{
				"payload.bin": {0x01, 0x02},
			},
		},
	)

	got, err := resolveVolumes(ctx, kubeClient, &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Volumes: []corev1.Volume{
				{
					Name: "bin",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "bin"},
						},
					},
				},
			},
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "bin", MountPath: "/data"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveVolumes failed: %v", err)
	}

	want := []*ateletpb.ResolvedVolume{
		{
			Name: "bin",
			Files: []*ateletpb.VolumeFile{
				{RelativePath: "payload.bin", Content: []byte{0x01, 0x02}, Mode: defaultConfigMapFileMode},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Fatalf("ResolvedVolume mismatch (-want +got):\n%s", diff)
	}
}

func TestProjectedByteFilesPerItemMode(t *testing.T) {
	modeA := int32(0o600)
	got, err := projectedByteFiles(map[string][]byte{
		"a": []byte("a"),
		"b": []byte("b"),
	}, []corev1.KeyToPath{
		{Key: "a", Path: "a", Mode: &modeA},
		{Key: "b", Path: "b"},
	}, defaultConfigMapFileMode)
	if err != nil {
		t.Fatalf("projectedByteFiles failed: %v", err)
	}

	want := []*ateletpb.VolumeFile{
		{RelativePath: "a", Content: []byte("a"), Mode: 0o600},
		{RelativePath: "b", Content: []byte("b"), Mode: defaultConfigMapFileMode},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Fatalf("VolumeFile mismatch (-want +got):\n%s", diff)
	}
}

func TestProjectedByteFilesMissingKey(t *testing.T) {
	_, err := projectedByteFiles(map[string][]byte{
		"present": []byte("ok"),
	}, []corev1.KeyToPath{
		{Key: "missing", Path: "missing"},
	}, defaultConfigMapFileMode)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v: %v", status.Code(err), err)
	}
}

func TestValidateRelativeVolumePathRejectsAbsolute(t *testing.T) {
	if err := validateRelativeVolumePath("/etc/passwd"); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
}
