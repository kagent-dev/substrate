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
	"fmt"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func workloadSpecFromActorTemplate(ctx context.Context, kubeClient kubernetes.Interface, actorTemplate *atev1alpha1.ActorTemplate) (*ateletpb.WorkloadSpec, error) {
	workloadSpec := &ateletpb.WorkloadSpec{
		PauseImage: actorTemplate.Spec.PauseImage,
	}
	resolver := envResolver{
		kubeClient: kubeClient,
		namespace:  actorTemplate.Namespace,
		configMaps: map[string]*corev1.ConfigMap{},
		secrets:    map[string]*corev1.Secret{},
	}

	for _, ctr := range actorTemplate.Spec.Containers {
		ateletCtr := &ateletpb.Container{
			Name:    ctr.Name,
			Image:   ctr.Image,
			Command: ctr.Command,
		}
		for _, env := range ctr.Env {
			ateletEnv, err := resolver.resolve(ctx, ctr.Name, env)
			if err != nil {
				return nil, err
			}
			if ateletEnv != nil {
				ateletCtr.Env = append(ateletCtr.Env, ateletEnv)
			}
		}
		workloadSpec.Containers = append(workloadSpec.Containers, ateletCtr)
	}

	return workloadSpec, nil
}

type envResolver struct {
	kubeClient kubernetes.Interface
	namespace  string
	configMaps map[string]*corev1.ConfigMap
	secrets    map[string]*corev1.Secret
}

func (r *envResolver) resolve(ctx context.Context, containerName string, env corev1.EnvVar) (*ateletpb.EnvEntry, error) {
	envID := fmt.Sprintf("container %q env %q", containerName, env.Name)
	if env.ValueFrom == nil {
		return &ateletpb.EnvEntry{
			Name:  env.Name,
			Value: env.Value,
		}, nil
	}

	if env.Value != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s sets both value and valueFrom", envID)
	}

	sources := valueFromSourceCount(env.ValueFrom)
	if sources == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "%s valueFrom does not set a source", envID)
	}
	if sources > 1 {
		return nil, status.Errorf(codes.FailedPrecondition, "%s combines multiple valueFrom sources", envID)
	}

	value, include, err := r.resolveValueFrom(ctx, envID, env.ValueFrom)
	if err != nil {
		return nil, err
	}
	if !include {
		return nil, nil
	}

	return &ateletpb.EnvEntry{
		Name:  env.Name,
		Value: value,
	}, nil
}

func (r *envResolver) resolveValueFrom(ctx context.Context, envID string, valueFrom *corev1.EnvVarSource) (string, bool, error) {
	if ref := valueFrom.ConfigMapKeyRef; ref != nil {
		return r.resolveConfigMapKeyRef(ctx, envID, ref)
	}
	if ref := valueFrom.SecretKeyRef; ref != nil {
		return r.resolveSecretKeyRef(ctx, envID, ref)
	}
	return "", false, status.Errorf(codes.FailedPrecondition, "%s uses unsupported valueFrom source; only configMapKeyRef and secretKeyRef are supported", envID)
}

func valueFromSourceCount(src *corev1.EnvVarSource) int {
	count := 0
	if src.ConfigMapKeyRef != nil {
		count++
	}
	if src.SecretKeyRef != nil {
		count++
	}
	if src.FieldRef != nil {
		count++
	}
	if src.ResourceFieldRef != nil {
		count++
	}
	if src.FileKeyRef != nil {
		count++
	}
	return count
}

func (r *envResolver) resolveConfigMapKeyRef(ctx context.Context, envID string, ref *corev1.ConfigMapKeySelector) (string, bool, error) {
	if ref.Name == "" {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s configMapKeyRef.name is required", envID)
	}
	if ref.Key == "" {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s configMapKeyRef.key is required", envID)
	}
	if r.kubeClient == nil {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s cannot resolve configMapKeyRef because Kubernetes client is unavailable", envID)
	}

	configMap, err := r.configMap(ctx, ref.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isOptional(ref.Optional) {
				return "", false, nil
			}
			return "", false, status.Errorf(codes.FailedPrecondition, "%s references missing ConfigMap %s/%s", envID, r.namespace, ref.Name)
		}
		return "", false, fmt.Errorf("while resolving %s configMapKeyRef %s/%s: %w", envID, r.namespace, ref.Name, err)
	}

	value, ok := configMap.Data[ref.Key]
	if !ok {
		if isOptional(ref.Optional) {
			return "", false, nil
		}
		return "", false, status.Errorf(codes.FailedPrecondition, "%s references missing key %q in ConfigMap %s/%s", envID, ref.Key, r.namespace, ref.Name)
	}

	return value, true, nil
}

func (r *envResolver) resolveSecretKeyRef(ctx context.Context, envID string, ref *corev1.SecretKeySelector) (string, bool, error) {
	if ref.Name == "" {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s secretKeyRef.name is required", envID)
	}
	if ref.Key == "" {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s secretKeyRef.key is required", envID)
	}
	if r.kubeClient == nil {
		return "", false, status.Errorf(codes.FailedPrecondition, "%s cannot resolve secretKeyRef because Kubernetes client is unavailable", envID)
	}

	secret, err := r.secret(ctx, ref.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isOptional(ref.Optional) {
				return "", false, nil
			}
			return "", false, status.Errorf(codes.FailedPrecondition, "%s references missing secret %s/%s", envID, r.namespace, ref.Name)
		}
		return "", false, fmt.Errorf("while resolving %s secretKeyRef %s/%s: %w", envID, r.namespace, ref.Name, err)
	}

	value, ok := secret.Data[ref.Key]
	if !ok {
		if isOptional(ref.Optional) {
			return "", false, nil
		}
		return "", false, status.Errorf(codes.FailedPrecondition, "%s references missing key %q in secret %s/%s", envID, ref.Key, r.namespace, ref.Name)
	}

	return string(value), true, nil
}

func (r *envResolver) configMap(ctx context.Context, name string) (*corev1.ConfigMap, error) {
	if configMap, ok := r.configMaps[name]; ok {
		return configMap, nil
	}

	configMap, err := r.kubeClient.CoreV1().ConfigMaps(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r.configMaps[name] = configMap
	return configMap, nil
}

func (r *envResolver) secret(ctx context.Context, name string) (*corev1.Secret, error) {
	if secret, ok := r.secrets[name]; ok {
		return secret, nil
	}

	secret, err := r.kubeClient.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r.secrets[name] = secret
	return secret, nil
}

func isOptional(optional *bool) bool {
	return optional != nil && *optional
}
