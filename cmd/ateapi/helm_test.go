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
	"fmt"
	"github.com/agent-substrate/substrate/pkg/postgressetup"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestHelmAPIServerFlags(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "test", "../../charts/substrate", "-n", "custom").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	found := false
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Kind != "Deployment" {
			continue
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "ate-api-server" {
				continue
			}
			found = true
			for _, arg := range container.Args {
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
				if pflag.Lookup(name) == nil {
					t.Errorf("chart passes unknown ateapi flag %q", name)
				}
			}
		}
	}
	if !found {
		t.Fatal("missing API server deployment")
	}
}

func TestHelmPostgresCertificates(t *testing.T) {
	for _, tc := range []struct {
		release, namespace     string
		external, certificates bool
	}{
		{release: "substrate", namespace: "ate-system"},
		{release: "substrate", namespace: "ate-system", certificates: true},
		{release: "team", namespace: "custom", certificates: true},
		{release: "team", namespace: "custom", external: true},
	} {
		t.Run(fmt.Sprintf("%s/%s/external=%t/certificates=%t", tc.release, tc.namespace, tc.external, tc.certificates), func(t *testing.T) {
			args := []string{"template", tc.release, "../../charts/substrate", "-n", tc.namespace}
			if tc.certificates {
				args = append(args, "--set", "postgres.clientCertificates.enabled=true")
			}
			if tc.external {
				args = append(args, "--set", "postgres.readWriteConnectionStringSecretRef.name=external-runtime", "--set", "postgres.ownerConnectionStringSecretRef.name=external-owner")
			}
			out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			serviceAccount := "ate-api-server"
			if tc.release != "substrate" {
				serviceAccount = tc.release + "-ate-api-server"
			}
			certOut, err := exec.CommandContext(t.Context(), "helm", "template", "substrate-podcert", "../../charts/substrate-podcert", "-n", "certificate-system", "--set", "postgresClientNamespace="+tc.namespace, "--set", "postgresClientServiceAccount="+serviceAccount).CombinedOutput()
			if err != nil {
				t.Fatalf("render controller: %v\n%s", err, certOut)
			}
			out = append(out, append([]byte("\n---\n"), certOut...)...)
			var api, controller corev1.PodSpec
			var signerPermission bool
			for _, doc := range strings.Split(string(out), "\n---\n") {
				var deployment appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
					t.Fatal(err)
				}
				if deployment.Kind == "Deployment" {
					switch deployment.Spec.Template.Labels["app"] {
					case "ate-api-server":
						api = deployment.Spec.Template.Spec
					case "podcertificate-controller":
						controller = deployment.Spec.Template.Spec
					}
				}
				if deployment.Kind == "ClusterRole" {
					var role rbacv1.ClusterRole
					if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
						t.Fatal(err)
					}
					for _, rule := range role.Rules {
						if slices.Contains(rule.Resources, "signers") && slices.Contains(rule.ResourceNames, "postgres.podcert.ate.dev/*") && slices.Contains(rule.Verbs, "sign") && slices.Contains(rule.Verbs, "attest") {
							signerPermission = true
						}
					}
				}
			}
			if api.ServiceAccountName == "" || len(controller.Containers) != 1 || !signerPermission {
				t.Fatal("missing API identity, certificate controller, or PostgreSQL signer permission")
			}
			for _, arg := range []string{
				"--postgres-client-namespace=" + tc.namespace,
				"--postgres-client-service-account=" + api.ServiceAccountName,
				"--postgres-ca-pool=/run/ca-state/postgres-pool.json",
			} {
				if !slices.Contains(controller.Containers[0].Args, arg) {
					t.Errorf("controller is missing %s", arg)
				}
			}
			var caSecret bool
			for _, volume := range controller.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil && source.Secret.Name == "postgres-ca-pool" {
						caSecret = slices.Contains(source.Secret.Items, corev1.KeyToPath{Key: "pool", Path: "postgres-pool.json"})
					}
				}
			}
			if !caSecret {
				t.Error("controller is missing its PostgreSQL CA pool")
			}
			bundles := map[string]string{}
			for _, volume := range api.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					cert := source.PodCertificate
					if cert != nil && cert.SignerName == "postgres.podcert.ate.dev/identity" {
						bundles[cert.CredentialBundlePath] = cert.UserAnnotations["postgres.podcert.ate.dev/username"]
					}
				}
			}
			var mounted bool
			for _, container := range api.Containers {
				if container.Name == "ate-api-server" {
					for _, mount := range container.VolumeMounts {
						mounted = mounted || (mount.Name == "postgres" && mount.MountPath == "/run/postgres.podcert.ate.dev" && mount.ReadOnly)
					}
				}
			}
			if !tc.certificates {
				if len(bundles) != 0 || mounted {
					t.Fatal("disabled PostgreSQL certificates must have no projections or mount")
				}
				for _, volume := range api.Volumes {
					if volume.Name == "postgres" {
						t.Error("disabled PostgreSQL certificates must have no postgres volume")
					}
				}
				return
			}
			if len(bundles) != 2 {
				t.Fatalf("got %d PostgreSQL certificate projections, want two", len(bundles))
			}
			for _, user := range []string{postgressetup.OwnerUser, postgressetup.ReadWriteUser} {
				if bundles[user+".pem"] != user {
					t.Errorf("missing separate certificate for %s", user)
				}
			}

			if !mounted {
				t.Error("API server cannot read its projected PostgreSQL certificates")
			}
		})
	}
}

// Snapshot transfers must reach the same backend from both plugin processes.
// The API rejects storage settings on its own container after the plugin split.
func TestHelmSnapshotPlugins(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
			args := []string{"template", "test", "../../charts/substrate", "-n", "custom"}
			issuer := "https://idp.custom.svc"
			if external {
				issuer = "https://actors.example.com"
				args = append(args, "--set", "rustfs.enabled=false", "--set", "atelet.storageBackend=gcs",
					"--set", "ateApi.actorJWTIssuer="+issuer,
					"--set", "snapshotPlugin.extraEnv[0].name=GOOGLE_APPLICATION_CREDENTIALS",
					"--set", "snapshotPlugin.extraEnv[0].value=/credentials/key.json")
			}
			out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			found := map[string]bool{}
			for _, doc := range strings.Split(string(out), "\n---\n") {
				var deployment appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
					t.Fatal(err)
				}
				if deployment.Kind == "ConfigMap" && deployment.Name == "ate-api-server-envvars" {
					var config corev1.ConfigMap
					if err := yaml.Unmarshal([]byte(doc), &config); err != nil {
						t.Fatal(err)
					}
					if got := config.Data["ATE_API_ACTOR_JWT_ISSUER"]; got != issuer {
						t.Errorf("issuer = %q, want %q", got, issuer)
					}
					found["issuer"] = true
				}
				mode, app := "control", "ate-api-server"
				if deployment.Kind == "DaemonSet" {
					mode, app = "node", "atelet"
				} else if deployment.Kind != "Deployment" || deployment.Name != "test-ate-api-server" {
					continue
				}
				pod := deployment.Spec.Template.Spec
				if len(pod.InitContainers) != 1 {
					t.Fatalf("%s: expected one snapshot sidecar, got %v", mode, pod.InitContainers)
				}
				plugin := pod.InitContainers[0]
				socketArg := "--socket=/run/snapshot-plugin/" + mode + ".sock"
				if plugin.Name != "snapshot-plugin" || plugin.RestartPolicy == nil || *plugin.RestartPolicy != corev1.ContainerRestartPolicyAlways || !slices.Contains(plugin.Args, mode) || !slices.Contains(plugin.Args, socketArg) {
					t.Fatalf("%s: invalid snapshot sidecar: %+v", mode, plugin)
				}
				if plugin.StartupProbe == nil || plugin.StartupProbe.Exec == nil || !slices.Contains(plugin.StartupProbe.Exec.Command, socketArg) {
					t.Errorf("%s: startup probe does not check the plugin socket", mode)
				}
				env := map[string]string{}
				for _, entry := range plugin.Env {
					env[entry.Name] = entry.Value
				}
				if external {
					if env["ATE_STORAGE_BACKEND"] != "gcs" || env["GOOGLE_APPLICATION_CREDENTIALS"] != "/credentials/key.json" || env["AWS_ENDPOINT_URL"] != "" {
						t.Errorf("%s: external storage settings = %v", mode, env)
					}
				} else if env["ATE_STORAGE_BACKEND"] != "s3" || env["AWS_ENDPOINT_URL"] != "http://test-rustfs.custom.svc:9000" || env["AWS_ACCESS_KEY_ID"] == "" || env["AWS_SECRET_ACCESS_KEY"] == "" {
					t.Errorf("%s: RustFS storage settings = %v", mode, env)
				}
				if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "snapshot-plugin" && v.EmptyDir != nil }) {
					t.Errorf("%s: missing shared socket volume", mode)
				}
				for _, container := range append(pod.Containers, plugin) {
					if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
						return m.Name == "snapshot-plugin" && m.MountPath == "/run/snapshot-plugin"
					}) {
						t.Errorf("%s: %s does not mount the shared socket", mode, container.Name)
					}
					if container.Name == app && !slices.Contains(container.Args, "--snapshot-plugin-socket=/run/snapshot-plugin/"+mode+".sock") {
						t.Errorf("%s: application does not use the plugin socket", mode)
					}
					if container.Name == "ate-api-server" && slices.ContainsFunc(container.Env, func(e corev1.EnvVar) bool { return e.Name == "ATE_STORAGE_BACKEND" }) {
						t.Error("API server still receives ATE_STORAGE_BACKEND")
					}
				}
				if mode == "node" && (!slices.Contains(plugin.Args, "--root=/var/lib/ate") || !slices.ContainsFunc(plugin.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "run-ateom" && m.MountPath == "/var/lib/ate" })) {
					t.Error("node plugin cannot access snapshot files")
				}
				found[mode] = true
			}
			for _, name := range []string{"issuer", "control", "node"} {
				if !found[name] {
					t.Errorf("missing %s configuration", name)
				}
			}
		})
	}
}

// These resources are excluded from the general chart render check, but must
// use the same version schema and validation as the upstream installation.
func TestHelmSandboxConfigParity(t *testing.T) {
	for _, name := range []string{"sandboxconfig-gvisor.yaml", "sandboxconfig-validation.yaml"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "helm", "template", "test", "../../charts/substrate", "--show-only", "templates/"+name).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			manifest, err := os.ReadFile("../../manifests/ate-install/" + name)
			if err != nil {
				t.Fatal(err)
			}
			parse := func(data string) []map[string]any {
				var result []map[string]any
				for _, doc := range strings.Split(data, "\n---\n") {
					var resource map[string]any
					if err := yaml.Unmarshal([]byte(doc), &resource); err != nil {
						t.Fatal(err)
					}
					if len(resource) > 0 {
						result = append(result, resource)
					}
				}
				return result
			}
			if !reflect.DeepEqual(parse(string(out)), parse(string(manifest))) {
				t.Error("chart sandbox configuration differs from the upstream manifest")
			}
		})
	}
}
