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
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultConfigMapFileMode = 0o644
	defaultSecretFileMode    = 0o644
)

type volumeResolver struct {
	kubeClient kubernetes.Interface
	namespace  string
	configMaps map[string]*corev1.ConfigMap
	secrets    map[string]*corev1.Secret
}

func resolveVolumes(ctx context.Context, kubeClient kubernetes.Interface, actorTemplate *atev1alpha1.ActorTemplate) ([]*ateletpb.ResolvedVolume, error) {
	if actorTemplate == nil {
		return nil, fmt.Errorf("actor template is required")
	}
	if err := validateVolumeMounts(actorTemplate); err != nil {
		return nil, err
	}
	if len(actorTemplate.Spec.Volumes) == 0 {
		return nil, nil
	}

	resolver := volumeResolver{
		kubeClient: kubeClient,
		namespace:  actorTemplate.Namespace,
		configMaps: map[string]*corev1.ConfigMap{},
		secrets:    map[string]*corev1.Secret{},
	}

	out := make([]*ateletpb.ResolvedVolume, 0, len(actorTemplate.Spec.Volumes))
	for _, vol := range actorTemplate.Spec.Volumes {
		name := strings.TrimSpace(vol.Name)
		if name == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "volume name is required")
		}
		resolved, err := resolver.resolveVolume(ctx, name, vol)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func validateVolumeMounts(actorTemplate *atev1alpha1.ActorTemplate) error {
	volumes := map[string]struct{}{}
	for _, vol := range actorTemplate.Spec.Volumes {
		name := strings.TrimSpace(vol.Name)
		if name == "" {
			return status.Errorf(codes.FailedPrecondition, "volume name is required")
		}
		if _, ok := volumes[name]; ok {
			return status.Errorf(codes.FailedPrecondition, "duplicate volume name %q", name)
		}
		volumes[name] = struct{}{}
		if err := validateVolumeSource(name, vol); err != nil {
			return err
		}
	}

	for _, ctr := range actorTemplate.Spec.Containers {
		for _, mount := range ctr.VolumeMounts {
			mountID := fmt.Sprintf("container %q volumeMount %q", ctr.Name, mount.Name)
			if mount.Name == "" {
				return status.Errorf(codes.FailedPrecondition, "%s: name is required", mountID)
			}
			if _, ok := volumes[mount.Name]; !ok {
				return status.Errorf(codes.FailedPrecondition, "%s: references undefined volume %q", mountID, mount.Name)
			}
			if err := validateMountPath(mount.MountPath); err != nil {
				return status.Errorf(codes.FailedPrecondition, "%s: %v", mountID, err)
			}
			if mount.SubPath != "" {
				if err := validateRelativeVolumePath(mount.SubPath); err != nil {
					return status.Errorf(codes.FailedPrecondition, "%s subPath: %v", mountID, err)
				}
			}
		}
	}
	return nil
}

func validateVolumeSource(name string, vol corev1.Volume) error {
	sources := 0
	if vol.ConfigMap != nil {
		sources++
	}
	if vol.Secret != nil {
		sources++
	}
	if vol.EmptyDir != nil {
		sources++
	}
	if vol.HostPath != nil || vol.PersistentVolumeClaim != nil || vol.Projected != nil || vol.CSI != nil || vol.NFS != nil {
		return status.Errorf(codes.FailedPrecondition, "volume %q uses an unsupported volume source", name)
	}
	if sources == 0 {
		return status.Errorf(codes.FailedPrecondition, "volume %q must set configMap, secret, or emptyDir", name)
	}
	if sources > 1 {
		return status.Errorf(codes.FailedPrecondition, "volume %q combines multiple volume sources", name)
	}
	return nil
}

func validateMountPath(mountPath string) error {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		return fmt.Errorf("mountPath is required")
	}
	if !filepath.IsAbs(mountPath) {
		return fmt.Errorf("mountPath %q must be absolute", mountPath)
	}
	clean := filepath.Clean(mountPath)
	if clean != mountPath && clean+"/" != mountPath {
		return fmt.Errorf("mountPath %q must be clean", mountPath)
	}
	return nil
}

func validateRelativeVolumePath(rel string) error {
	rel = path.Clean(rel)
	if path.IsAbs(rel) {
		return fmt.Errorf("path %q must be relative", rel)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return fmt.Errorf("path %q must stay within the volume root", rel)
	}
	return nil
}

func (r *volumeResolver) resolveVolume(ctx context.Context, name string, vol corev1.Volume) (*ateletpb.ResolvedVolume, error) {
	switch {
	case vol.ConfigMap != nil:
		files, err := r.resolveConfigMapVolume(ctx, name, vol.ConfigMap)
		if err != nil {
			return nil, err
		}
		return &ateletpb.ResolvedVolume{Name: name, Files: files}, nil
	case vol.Secret != nil:
		files, err := r.resolveSecretVolume(ctx, name, vol.Secret)
		if err != nil {
			return nil, err
		}
		return &ateletpb.ResolvedVolume{Name: name, Files: files}, nil
	case vol.EmptyDir != nil:
		return &ateletpb.ResolvedVolume{Name: name}, nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "volume %q uses an unsupported volume source", name)
	}
}

func (r *volumeResolver) resolveConfigMapVolume(ctx context.Context, volumeName string, src *corev1.ConfigMapVolumeSource) ([]*ateletpb.VolumeFile, error) {
	if src == nil || strings.TrimSpace(src.Name) == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %q configMap.name is required", volumeName)
	}
	cm, err := r.configMap(ctx, src.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isOptional(src.Optional) {
				return nil, nil
			}
			return nil, status.Errorf(codes.FailedPrecondition, "volume %q references missing configMap %s/%s", volumeName, r.namespace, src.Name)
		}
		return nil, status.Errorf(codes.Internal, "get configMap %s/%s: %v", r.namespace, src.Name, err)
	}
	mode := defaultConfigMapFileMode
	if src.DefaultMode != nil {
		mode = int(*src.DefaultMode)
	}
	data := map[string][]byte{}
	for k, v := range cm.Data {
		data[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		data[k] = append([]byte(nil), v...)
	}
	return projectedByteFiles(data, src.Items, mode)
}

func (r *volumeResolver) resolveSecretVolume(ctx context.Context, volumeName string, src *corev1.SecretVolumeSource) ([]*ateletpb.VolumeFile, error) {
	if src == nil || strings.TrimSpace(src.SecretName) == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %q secret.secretName is required", volumeName)
	}
	secret, err := r.secret(ctx, src.SecretName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isOptional(src.Optional) {
				return nil, nil
			}
			return nil, status.Errorf(codes.FailedPrecondition, "volume %q references missing secret %s/%s", volumeName, r.namespace, src.SecretName)
		}
		return nil, status.Errorf(codes.Internal, "get secret %s/%s: %v", r.namespace, src.SecretName, err)
	}
	mode := defaultSecretFileMode
	if src.DefaultMode != nil {
		mode = int(*src.DefaultMode)
	}
	data := map[string][]byte{}
	for k, v := range secret.Data {
		data[k] = append([]byte(nil), v...)
	}
	return projectedByteFiles(data, src.Items, mode)
}

func projectedByteFiles(data map[string][]byte, items []corev1.KeyToPath, mode int) ([]*ateletpb.VolumeFile, error) {
	if len(items) == 0 {
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]*ateletpb.VolumeFile, 0, len(keys))
		for _, key := range keys {
			if err := validateRelativeVolumePath(key); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "invalid configMap key %q: %v", key, err)
			}
			out = append(out, &ateletpb.VolumeFile{
				RelativePath: filepath.ToSlash(key),
				Content:      append([]byte(nil), data[key]...),
				Mode:         uint32(mode),
			})
		}
		return out, nil
	}

	out := make([]*ateletpb.VolumeFile, 0, len(items))
	for _, item := range items {
		if item.Key == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "volume item key is required")
		}
		rel := item.Path
		if rel == "" {
			rel = item.Key
		}
		if err := validateRelativeVolumePath(rel); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "invalid volume item path %q: %v", rel, err)
		}
		val, ok := data[item.Key]
		if !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "volume item references missing key %q", item.Key)
		}
		fileMode := mode
		if item.Mode != nil {
			fileMode = int(*item.Mode)
		}
		out = append(out, &ateletpb.VolumeFile{
			RelativePath: filepath.ToSlash(rel),
			Content:      append([]byte(nil), val...),
			Mode:         uint32(fileMode),
		})
	}
	return out, nil
}

func (r *volumeResolver) configMap(ctx context.Context, name string) (*corev1.ConfigMap, error) {
	if cm, ok := r.configMaps[name]; ok {
		return cm, nil
	}
	if r.kubeClient == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "kubernetes client is unavailable to resolve configMap %q", name)
	}
	cm, err := r.kubeClient.CoreV1().ConfigMaps(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r.configMaps[name] = cm
	return cm, nil
}

func (r *volumeResolver) secret(ctx context.Context, name string) (*corev1.Secret, error) {
	if secret, ok := r.secrets[name]; ok {
		return secret, nil
	}
	if r.kubeClient == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "kubernetes client is unavailable to resolve secret %q", name)
	}
	secret, err := r.kubeClient.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r.secrets[name] = secret
	return secret, nil
}
