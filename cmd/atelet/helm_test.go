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

package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestHelmImageCredentialProvider(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	for _, tc := range []struct {
		name, config, binDir string
		wantError            bool
	}{
		{name: "public registries"},
		{name: "node provider", config: "/etc/srv/kubernetes/cri_auth_config.yaml", binDir: "/home/kubernetes/bin"},
		{name: "missing bin directory", config: "/etc/provider.yaml", wantError: true},
		{name: "missing config", binDir: "/opt/providers", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "helm", "template", "substrate", "../../charts/substrate",
				"--show-only", "templates/atelet.yaml",
				"--set-string", "atelet.imageCredentialProviderConfig="+tc.config,
				"--set-string", "atelet.imageCredentialProviderBinDir="+tc.binDir).CombinedOutput()
			if tc.wantError {
				if err == nil || !strings.Contains(string(out), "must be set together") {
					t.Fatalf("render incomplete provider config: %v\n%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			var pod *corev1.PodSpec
			for _, doc := range strings.Split(string(out), "\n---\n") {
				var ds appsv1.DaemonSet
				if err := yaml.Unmarshal([]byte(doc), &ds); err != nil {
					t.Fatal(err)
				}
				if ds.Kind == "DaemonSet" {
					pod = &ds.Spec.Template.Spec
				}
			}
			if pod == nil || len(pod.Containers) != 1 {
				t.Fatal("expected atelet DaemonSet with one container")
			}
			container := pod.Containers[0]
			for _, arg := range container.Args {
				if strings.HasPrefix(arg, "--gcp-auth-for-image-pulls") {
					t.Fatal("chart passes the removed GCP authentication flag")
				}
			}
			for _, binding := range []struct{ name, flag, mount, host string }{
				{"image-credential-provider-config", "--image-credential-provider-config", "/run/image-credential-provider/config.yaml", tc.config},
				{"image-credential-provider-bin", "--image-credential-provider-bin-dir", "/run/image-credential-provider/bin", tc.binDir},
			} {
				enabled := tc.config != ""
				if slices.Contains(container.Args, binding.flag+"="+binding.mount) != enabled {
					t.Errorf("%s flag does not match provider configuration", binding.flag)
				}
				mountIndex := slices.IndexFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == binding.name })
				volumeIndex := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == binding.name })
				if (mountIndex >= 0) != enabled || (volumeIndex >= 0) != enabled {
					t.Fatalf("%s mount/volume presence does not match provider configuration", binding.name)
				}
				if enabled {
					mount := container.VolumeMounts[mountIndex]
					volume := pod.Volumes[volumeIndex]
					if mount.MountPath != binding.mount || !mount.ReadOnly || volume.HostPath == nil || volume.HostPath.Path != binding.host {
						t.Errorf("%s must mount the configured host path read-only at the flag's path", binding.name)
					}
				}
			}
		})
	}
}
