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
	"os/exec"
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
		release, namespace string
		external           bool
	}{
		{release: "substrate", namespace: "ate-system"},
		{release: "team", namespace: "custom"},
		{release: "team", namespace: "custom", external: true},
	} {
		t.Run(fmt.Sprintf("%s/%s/external=%t", tc.release, tc.namespace, tc.external), func(t *testing.T) {
			args := []string{"template", tc.release, "../../charts/substrate", "-n", tc.namespace}
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
			if len(bundles) != 2 {
				t.Fatalf("got %d PostgreSQL certificate projections, want two", len(bundles))
			}
			for _, user := range []string{postgressetup.OwnerUser, postgressetup.ReadWriteUser} {
				if bundles[user+".pem"] != user {
					t.Errorf("missing separate certificate for %s", user)
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
			if !mounted {
				t.Error("API server cannot read its projected PostgreSQL certificates")
			}
		})
	}
}
