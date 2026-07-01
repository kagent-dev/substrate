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

package controlapi

import (
	"fmt"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func protoActorTemplateToAPI(in *ateapipb.ActorTemplate) *atev1alpha1.ActorTemplate {
	if in == nil {
		return nil
	}
	out := &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: in.GetAtespace(),
			Name:      in.GetName(),
		},
		Spec:   protoActorTemplateSpecToAPI(in.GetSpec()),
		Status: protoActorTemplateStatusToAPI(in.GetStatus()),
	}
	return out
}

func apiActorTemplateToProto(in *atev1alpha1.ActorTemplate) *ateapipb.ActorTemplate {
	if in == nil {
		return nil
	}
	return &ateapipb.ActorTemplate{
		Atespace: in.Namespace,
		Name:     in.Name,
		Spec:     apiActorTemplateSpecToProto(in.Spec),
		Status:   apiActorTemplateStatusToProto(in.Status),
	}
}

func protoActorTemplateSpecToAPI(in *ateapipb.ActorTemplateSpec) atev1alpha1.ActorTemplateSpec {
	if in == nil {
		return atev1alpha1.ActorTemplateSpec{}
	}
	return atev1alpha1.ActorTemplateSpec{
		PauseImage:      in.GetPauseImage(),
		Containers:      protoContainersToAPI(in.GetContainers()),
		SnapshotsConfig: protoSnapshotsConfigToAPI(in.GetSnapshotsConfig()),
		SandboxClass:    atev1alpha1.SandboxClass(in.GetSandboxClass()),
		WorkerSelector:  protoLabelSelectorToAPI(in.GetWorkerSelector()),
		Volumes:         protoVolumesToAPI(in.GetVolumes()),
	}
}

func apiActorTemplateSpecToProto(in atev1alpha1.ActorTemplateSpec) *ateapipb.ActorTemplateSpec {
	return &ateapipb.ActorTemplateSpec{
		PauseImage:      in.PauseImage,
		Containers:      apiContainersToProto(in.Containers),
		SnapshotsConfig: apiSnapshotsConfigToProto(in.SnapshotsConfig),
		SandboxClass:    string(in.SandboxClass),
		WorkerSelector:  apiLabelSelectorToProto(in.WorkerSelector),
		Volumes:         apiVolumesToProto(in.Volumes),
	}
}

func protoActorTemplateStatusToAPI(in *ateapipb.ActorTemplateStatus) atev1alpha1.ActorTemplateStatus {
	if in == nil {
		return atev1alpha1.ActorTemplateStatus{}
	}
	out := atev1alpha1.ActorTemplateStatus{
		Phase:          atev1alpha1.PhaseType(in.GetPhase()),
		GoldenActorID:  in.GetGoldenActorId(),
		GoldenSnapshot: in.GetGoldenSnapshot(),
		Conditions:     protoConditionsToAPI(in.GetConditions()),
	}
	if in.GetTakeGoldenSnapshotAt() != nil {
		out.TakeGoldenSnapshotAt = metav1.NewTime(in.GetTakeGoldenSnapshotAt().AsTime())
	}
	return out
}

func apiActorTemplateStatusToProto(in atev1alpha1.ActorTemplateStatus) *ateapipb.ActorTemplateStatus {
	return &ateapipb.ActorTemplateStatus{
		Phase:                string(in.Phase),
		GoldenActorId:        in.GoldenActorID,
		TakeGoldenSnapshotAt: timestampProto(in.TakeGoldenSnapshotAt),
		GoldenSnapshot:       in.GoldenSnapshot,
		Conditions:           apiConditionsToProto(in.Conditions),
	}
}

func protoContainersToAPI(in []*ateapipb.Container) []atev1alpha1.Container {
	out := make([]atev1alpha1.Container, 0, len(in))
	for _, c := range in {
		out = append(out, atev1alpha1.Container{
			Name:         c.GetName(),
			Image:        c.GetImage(),
			Command:      append([]string(nil), c.GetCommand()...),
			Env:          protoEnvVarsToAPI(c.GetEnv()),
			Readyz:       protoReadyzToAPI(c.GetReadyz()),
			VolumeMounts: protoVolumeMountsToAPI(c.GetVolumeMounts()),
		})
	}
	return out
}

func apiContainersToProto(in []atev1alpha1.Container) []*ateapipb.Container {
	out := make([]*ateapipb.Container, 0, len(in))
	for i := range in {
		c := in[i]
		out = append(out, &ateapipb.Container{
			Name:         c.Name,
			Image:        c.Image,
			Command:      append([]string(nil), c.Command...),
			Env:          apiEnvVarsToProto(c.Env),
			Readyz:       apiReadyzToProto(c.Readyz),
			VolumeMounts: apiVolumeMountsToProto(c.VolumeMounts),
		})
	}
	return out
}

func protoEnvVarsToAPI(in []*ateapipb.EnvVar) []atev1alpha1.EnvVar {
	out := make([]atev1alpha1.EnvVar, 0, len(in))
	for _, env := range in {
		var value *string
		if env.Value != nil {
			value = ptrVal(env.GetValue())
		}
		out = append(out, atev1alpha1.EnvVar{
			Name:      env.GetName(),
			Value:     value,
			ValueFrom: protoEnvVarSourceToAPI(env.GetValueFrom()),
		})
	}
	return out
}

func apiEnvVarsToProto(in []atev1alpha1.EnvVar) []*ateapipb.EnvVar {
	out := make([]*ateapipb.EnvVar, 0, len(in))
	for i := range in {
		env := in[i]
		out = append(out, &ateapipb.EnvVar{
			Name:      env.Name,
			Value:     env.Value,
			ValueFrom: apiEnvVarSourceToProto(env.ValueFrom),
		})
	}
	return out
}

func protoEnvVarSourceToAPI(in *ateapipb.EnvVarSource) *atev1alpha1.EnvVarSource {
	if in == nil {
		return nil
	}
	return &atev1alpha1.EnvVarSource{SecretKeyRef: protoSecretKeySelectorToAPI(in.GetSecretKeyRef())}
}

func apiEnvVarSourceToProto(in *atev1alpha1.EnvVarSource) *ateapipb.EnvVarSource {
	if in == nil {
		return nil
	}
	return &ateapipb.EnvVarSource{SecretKeyRef: apiSecretKeySelectorToProto(in.SecretKeyRef)}
}

func protoSecretKeySelectorToAPI(in *ateapipb.SecretKeySelector) *atev1alpha1.SecretKeySelector {
	if in == nil {
		return nil
	}
	var optional *bool
	if in.Optional != nil {
		optional = ptrVal(in.GetOptional())
	}
	return &atev1alpha1.SecretKeySelector{Name: in.GetName(), Key: in.GetKey(), Optional: optional}
}

func apiSecretKeySelectorToProto(in *atev1alpha1.SecretKeySelector) *ateapipb.SecretKeySelector {
	if in == nil {
		return nil
	}
	return &ateapipb.SecretKeySelector{Name: in.Name, Key: in.Key, Optional: in.Optional}
}

func protoReadyzToAPI(in *ateapipb.ContainerReadyz) *atev1alpha1.ContainerReadyz {
	if in == nil {
		return nil
	}
	return &atev1alpha1.ContainerReadyz{HTTPGet: protoHTTPGetToAPI(in.GetHttpGet())}
}

func apiReadyzToProto(in *atev1alpha1.ContainerReadyz) *ateapipb.ContainerReadyz {
	if in == nil {
		return nil
	}
	return &ateapipb.ContainerReadyz{HttpGet: apiHTTPGetToProto(in.HTTPGet)}
}

func protoHTTPGetToAPI(in *ateapipb.HTTPGetAction) *atev1alpha1.HTTPGetAction {
	if in == nil {
		return nil
	}
	return &atev1alpha1.HTTPGetAction{Path: in.GetPath(), Port: in.GetPort()}
}

func apiHTTPGetToProto(in *atev1alpha1.HTTPGetAction) *ateapipb.HTTPGetAction {
	if in == nil {
		return nil
	}
	return &ateapipb.HTTPGetAction{Path: in.Path, Port: in.Port}
}

func protoVolumeMountsToAPI(in []*ateapipb.VolumeMount) []atev1alpha1.VolumeMount {
	out := make([]atev1alpha1.VolumeMount, 0, len(in))
	for _, vm := range in {
		out = append(out, atev1alpha1.VolumeMount{Name: vm.GetName(), MountPath: vm.GetMountPath()})
	}
	return out
}

func apiVolumeMountsToProto(in []atev1alpha1.VolumeMount) []*ateapipb.VolumeMount {
	out := make([]*ateapipb.VolumeMount, 0, len(in))
	for i := range in {
		out = append(out, &ateapipb.VolumeMount{Name: in[i].Name, MountPath: in[i].MountPath})
	}
	return out
}

func protoVolumesToAPI(in []*ateapipb.Volume) []atev1alpha1.Volume {
	out := make([]atev1alpha1.Volume, 0, len(in))
	for _, v := range in {
		vol := atev1alpha1.Volume{Name: v.GetName()}
		if v.GetVolumeSource().GetDurableDir() != nil {
			vol.VolumeSource.DurableDir = &atev1alpha1.DurableDirVolumeSource{}
		}
		out = append(out, vol)
	}
	return out
}

func apiVolumesToProto(in []atev1alpha1.Volume) []*ateapipb.Volume {
	out := make([]*ateapipb.Volume, 0, len(in))
	for i := range in {
		v := in[i]
		src := &ateapipb.VolumeSource{}
		if v.VolumeSource.DurableDir != nil {
			src.DurableDir = &ateapipb.DurableDirVolumeSource{}
		}
		out = append(out, &ateapipb.Volume{Name: v.Name, VolumeSource: src})
	}
	return out
}

func protoSnapshotsConfigToAPI(in *ateapipb.SnapshotsConfig) atev1alpha1.SnapshotsConfig {
	if in == nil {
		return atev1alpha1.SnapshotsConfig{}
	}
	return atev1alpha1.SnapshotsConfig{
		Location: in.GetLocation(),
		OnPause:  atev1alpha1.SnapshotScope(in.GetOnPause()),
		OnCommit: atev1alpha1.SnapshotScope(in.GetOnCommit()),
	}
}

func apiSnapshotsConfigToProto(in atev1alpha1.SnapshotsConfig) *ateapipb.SnapshotsConfig {
	return &ateapipb.SnapshotsConfig{
		Location: in.Location,
		OnPause:  string(in.OnPause),
		OnCommit: string(in.OnCommit),
	}
}

func protoLabelSelectorToAPI(in *ateapipb.LabelSelector) *metav1.LabelSelector {
	if in == nil {
		return nil
	}
	return &metav1.LabelSelector{
		MatchLabels:      copyStringMap(in.GetMatchLabels()),
		MatchExpressions: protoLabelSelectorRequirementsToAPI(in.GetMatchExpressions()),
	}
}

func apiLabelSelectorToProto(in *metav1.LabelSelector) *ateapipb.LabelSelector {
	if in == nil {
		return nil
	}
	return &ateapipb.LabelSelector{
		MatchLabels:      copyStringMap(in.MatchLabels),
		MatchExpressions: apiLabelSelectorRequirementsToProto(in.MatchExpressions),
	}
}

func protoLabelSelectorRequirementsToAPI(in []*ateapipb.LabelSelectorRequirement) []metav1.LabelSelectorRequirement {
	out := make([]metav1.LabelSelectorRequirement, 0, len(in))
	for _, req := range in {
		out = append(out, metav1.LabelSelectorRequirement{
			Key:      req.GetKey(),
			Operator: metav1.LabelSelectorOperator(req.GetOperator()),
			Values:   append([]string(nil), req.GetValues()...),
		})
	}
	return out
}

func apiLabelSelectorRequirementsToProto(in []metav1.LabelSelectorRequirement) []*ateapipb.LabelSelectorRequirement {
	out := make([]*ateapipb.LabelSelectorRequirement, 0, len(in))
	for i := range in {
		req := in[i]
		out = append(out, &ateapipb.LabelSelectorRequirement{
			Key:      req.Key,
			Operator: string(req.Operator),
			Values:   append([]string(nil), req.Values...),
		})
	}
	return out
}

func protoWorkerPoolToAPI(in *ateapipb.WorkerPool) (*atev1alpha1.WorkerPool, error) {
	if in == nil {
		return nil, nil
	}
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
		Spec:   spec,
		Status: atev1alpha1.WorkerPoolStatus{Replicas: in.GetStatus().GetReplicas()},
	}, nil
}

func apiWorkerPoolToProto(in *atev1alpha1.WorkerPool) *ateapipb.WorkerPool {
	if in == nil {
		return nil
	}
	return &ateapipb.WorkerPool{
		Namespace: in.Namespace,
		Name:      in.Name,
		Labels:    copyStringMap(in.Labels),
		Spec:      apiWorkerPoolSpecToProto(in.Spec),
		Status:    &ateapipb.WorkerPoolStatus{Replicas: in.Status.Replicas},
	}
}

func protoWorkerPoolSpecToAPI(in *ateapipb.WorkerPoolSpec) (atev1alpha1.WorkerPoolSpec, error) {
	if in == nil {
		return atev1alpha1.WorkerPoolSpec{}, nil
	}
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

func apiWorkerPoolSpecToProto(in atev1alpha1.WorkerPoolSpec) *ateapipb.WorkerPoolSpec {
	return &ateapipb.WorkerPoolSpec{
		Replicas:          in.Replicas,
		AteomImage:        in.AteomImage,
		Template:          apiWorkerPoolPodTemplateToProto(in.Template),
		SandboxClass:      string(in.SandboxClass),
		SandboxConfigName: in.SandboxConfigName,
	}
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

func apiWorkerPoolPodTemplateToProto(in *atev1alpha1.WorkerPoolPodTemplate) *ateapipb.WorkerPoolPodTemplate {
	if in == nil {
		return nil
	}
	return &ateapipb.WorkerPoolPodTemplate{
		NodeSelector:      copyStringMap(in.NodeSelector),
		Tolerations:       apiTolerationsToProto(in.Tolerations),
		PriorityClassName: in.PriorityClassName,
		NodeAffinity:      apiNodeAffinityToProto(in.NodeAffinity),
		Resources:         apiResourceRequirementsToProto(in.Resources),
	}
}

func protoTolerationsToAPI(in []*ateapipb.Toleration) []corev1.Toleration {
	out := make([]corev1.Toleration, 0, len(in))
	for _, t := range in {
		var seconds *int64
		if t.TolerationSeconds != nil {
			seconds = ptrVal(t.GetTolerationSeconds())
		}
		out = append(out, corev1.Toleration{
			Key:               t.GetKey(),
			Operator:          corev1.TolerationOperator(t.GetOperator()),
			Value:             t.GetValue(),
			Effect:            corev1.TaintEffect(t.GetEffect()),
			TolerationSeconds: seconds,
		})
	}
	return out
}

func apiTolerationsToProto(in []corev1.Toleration) []*ateapipb.Toleration {
	out := make([]*ateapipb.Toleration, 0, len(in))
	for i := range in {
		t := in[i]
		out = append(out, &ateapipb.Toleration{
			Key:               t.Key,
			Operator:          string(t.Operator),
			Value:             t.Value,
			Effect:            string(t.Effect),
			TolerationSeconds: t.TolerationSeconds,
		})
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

func apiNodeAffinityToProto(in *corev1.NodeAffinity) *ateapipb.NodeAffinity {
	if in == nil {
		return nil
	}
	return &ateapipb.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution:  apiNodeSelectorToProto(in.RequiredDuringSchedulingIgnoredDuringExecution),
		PreferredDuringSchedulingIgnoredDuringExecution: apiPreferredSchedulingTermsToProto(in.PreferredDuringSchedulingIgnoredDuringExecution),
	}
}

func protoNodeSelectorToAPI(in *ateapipb.NodeSelector) *corev1.NodeSelector {
	if in == nil {
		return nil
	}
	out := &corev1.NodeSelector{NodeSelectorTerms: make([]corev1.NodeSelectorTerm, 0, len(in.GetNodeSelectorTerms()))}
	for _, term := range in.GetNodeSelectorTerms() {
		out.NodeSelectorTerms = append(out.NodeSelectorTerms, protoNodeSelectorTermToAPI(term))
	}
	return out
}

func apiNodeSelectorToProto(in *corev1.NodeSelector) *ateapipb.NodeSelector {
	if in == nil {
		return nil
	}
	out := &ateapipb.NodeSelector{NodeSelectorTerms: make([]*ateapipb.NodeSelectorTerm, 0, len(in.NodeSelectorTerms))}
	for i := range in.NodeSelectorTerms {
		out.NodeSelectorTerms = append(out.NodeSelectorTerms, apiNodeSelectorTermToProto(in.NodeSelectorTerms[i]))
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

func apiPreferredSchedulingTermsToProto(in []corev1.PreferredSchedulingTerm) []*ateapipb.PreferredSchedulingTerm {
	out := make([]*ateapipb.PreferredSchedulingTerm, 0, len(in))
	for i := range in {
		out = append(out, &ateapipb.PreferredSchedulingTerm{Weight: in[i].Weight, Preference: apiNodeSelectorTermToProto(in[i].Preference)})
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

func apiNodeSelectorTermToProto(in corev1.NodeSelectorTerm) *ateapipb.NodeSelectorTerm {
	return &ateapipb.NodeSelectorTerm{
		MatchExpressions: apiNodeSelectorRequirementsToProto(in.MatchExpressions),
		MatchFields:      apiNodeSelectorRequirementsToProto(in.MatchFields),
	}
}

func protoNodeSelectorRequirementsToAPI(in []*ateapipb.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	out := make([]corev1.NodeSelectorRequirement, 0, len(in))
	for _, req := range in {
		out = append(out, corev1.NodeSelectorRequirement{
			Key:      req.GetKey(),
			Operator: corev1.NodeSelectorOperator(req.GetOperator()),
			Values:   append([]string(nil), req.GetValues()...),
		})
	}
	return out
}

func apiNodeSelectorRequirementsToProto(in []corev1.NodeSelectorRequirement) []*ateapipb.NodeSelectorRequirement {
	out := make([]*ateapipb.NodeSelectorRequirement, 0, len(in))
	for i := range in {
		req := in[i]
		out = append(out, &ateapipb.NodeSelectorRequirement{Key: req.Key, Operator: string(req.Operator), Values: append([]string(nil), req.Values...)})
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

func apiResourceRequirementsToProto(in *corev1.ResourceRequirements) *ateapipb.ResourceRequirements {
	if in == nil {
		return nil
	}
	return &ateapipb.ResourceRequirements{
		Limits:   apiResourceListToProto(in.Limits),
		Requests: apiResourceListToProto(in.Requests),
	}
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

func apiResourceListToProto(in corev1.ResourceList) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		out[string(k)] = v.String()
	}
	return out
}

func protoSandboxConfigToAPI(in *ateapipb.SandboxConfig) *atev1alpha1.SandboxConfig {
	if in == nil {
		return nil
	}
	return &atev1alpha1.SandboxConfig{
		ObjectMeta: metav1.ObjectMeta{Name: in.GetName()},
		Spec: atev1alpha1.SandboxConfigSpec{
			SandboxClass: atev1alpha1.SandboxClass(in.GetSpec().GetSandboxClass()),
			Default:      in.GetSpec().GetDefault(),
			Assets:       protoSandboxAssetsToAPI(in.GetSpec().GetAssets()),
		},
	}
}

func apiSandboxConfigToProto(in *atev1alpha1.SandboxConfig) *ateapipb.SandboxConfig {
	if in == nil {
		return nil
	}
	return &ateapipb.SandboxConfig{
		Name: in.Name,
		Spec: &ateapipb.SandboxConfigSpec{
			SandboxClass: string(in.Spec.SandboxClass),
			Default:      in.Spec.Default,
			Assets:       apiSandboxAssetsToProto(in.Spec.Assets),
		},
	}
}

func protoSandboxAssetsToAPI(in map[string]*ateapipb.SandboxAssetFiles) map[string]map[string]atev1alpha1.AssetFile {
	if len(in) == 0 {
		return nil
	}
	out := map[string]map[string]atev1alpha1.AssetFile{}
	for arch, files := range in {
		out[arch] = map[string]atev1alpha1.AssetFile{}
		for name, file := range files.GetFiles() {
			out[arch][name] = atev1alpha1.AssetFile{URL: file.GetUrl(), SHA256: file.GetSha256()}
		}
	}
	return out
}

func apiSandboxAssetsToProto(in map[string]map[string]atev1alpha1.AssetFile) map[string]*ateapipb.SandboxAssetFiles {
	if len(in) == 0 {
		return nil
	}
	out := map[string]*ateapipb.SandboxAssetFiles{}
	for arch, files := range in {
		out[arch] = &ateapipb.SandboxAssetFiles{Files: map[string]*ateapipb.AssetFile{}}
		for name, file := range files {
			out[arch].Files[name] = &ateapipb.AssetFile{Url: file.URL, Sha256: file.SHA256}
		}
	}
	return out
}

func protoConditionsToAPI(in []*ateapipb.Condition) []metav1.Condition {
	out := make([]metav1.Condition, 0, len(in))
	for _, c := range in {
		cond := metav1.Condition{
			Type:               c.GetType(),
			Status:             metav1.ConditionStatus(c.GetStatus()),
			ObservedGeneration: c.GetObservedGeneration(),
			Reason:             c.GetReason(),
			Message:            c.GetMessage(),
		}
		if c.GetLastTransitionTime() != nil {
			cond.LastTransitionTime = metav1.NewTime(c.GetLastTransitionTime().AsTime())
		}
		out = append(out, cond)
	}
	return out
}

func apiConditionsToProto(in []metav1.Condition) []*ateapipb.Condition {
	out := make([]*ateapipb.Condition, 0, len(in))
	for i := range in {
		c := in[i]
		out = append(out, &ateapipb.Condition{
			Type:               c.Type,
			Status:             string(c.Status),
			ObservedGeneration: c.ObservedGeneration,
			LastTransitionTime: timestampProto(c.LastTransitionTime),
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}
	return out
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

func ptrVal[T any](v T) *T {
	return &v
}

func timestampProto(t metav1.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.Time)
}
