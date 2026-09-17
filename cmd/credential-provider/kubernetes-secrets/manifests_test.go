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
	"bytes"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestProviderManifests(t *testing.T) {
	for _, tc := range []struct {
		name, tool, namespace, prefix string
		image                         string
		args                          []string
		enabled                       bool
	}{
		{name: "disabled", tool: "helm", namespace: "ate-system", args: []string{"template", "substrate", "../../../charts/substrate", "-n", "ate-system"}},
		{name: "custom release", tool: "helm", namespace: "custom", prefix: "test-", enabled: true,
			args: []string{"template", "test", "../../../charts/substrate", "-n", "custom", "--set", "credentialProvider.enabled=true", "--set", "credentialProvider.namespacePolicies[0].atespace=team-a", "--set", "credentialProvider.namespacePolicies[0].allowedNamespaces[0]=ns1"}},
		{name: "kustomize", tool: "kubectl", namespace: "ate-system", enabled: true,
			args: []string{"kustomize", "../../../manifests/egress-credential-injection"}},
		{name: "CI images", tool: "helm", namespace: "ate-system", enabled: true,
			image: "localhost:5001/kagent-dev/substrate/kubernetes-secrets:helm-e2e",
			args:  []string{"template", "substrate", "../../../charts/substrate", "-n", "ate-system", "--set", "credentialProvider.enabled=true", "--set", "image.registry=localhost:5001", "--set", "image.tag=helm-e2e"}},
		{name: "global images", tool: "helm", namespace: "ate-system", enabled: true,
			image: "mirror.example/custom/substrate/kubernetes-secrets:test",
			args: []string{"template", "substrate", "../../../charts/substrate", "-n", "ate-system", "--set", "credentialProvider.enabled=true",
				"--set", "image.repository=custom/substrate", "--set", "image.tag=test", "--set", "global.imageRegistry=mirror.example",
				"--set", "imagePullSecrets[0].name=local", "--set", "global.imagePullSecrets[0].name=global", "--set", "global.imagePullPolicy=Always"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.tool); err != nil {
				t.Skipf("%s is not installed", tc.tool)
			}
			data, err := exec.CommandContext(t.Context(), tc.tool, tc.args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, data)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			var providerFound, portFound, policyFound, accountFound bool
			for {
				var doc struct {
					Kind     string
					Metadata metav1.ObjectMeta
					Spec     struct {
						Template corev1.PodTemplateSpec
						Ports    []corev1.ServicePort
					}
					Data  map[string]string
					Rules []rbacv1.PolicyRule
				}
				if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch doc.Kind {
				case "ServiceAccount":
					if doc.Metadata.Name == tc.prefix+"k8s-credential-provider" {
						accountFound = true
					}
				case "Deployment":
					if doc.Metadata.Name != tc.prefix+"k8s-credential-provider" {
						continue
					}
					pod := doc.Spec.Template.Spec
					if pod.ServiceAccountName != tc.prefix+"k8s-credential-provider" {
						t.Fatalf("unexpected ServiceAccount %q", pod.ServiceAccountName)
					}
					for _, container := range pod.Containers {
						if container.Name != "k8s-credential-provider" {
							continue
						}
						providerFound = true
						if tc.image != "" && container.Image != tc.image {
							t.Errorf("image = %q, want %q", container.Image, tc.image)
						}
						if tc.name == "global images" {
							if container.ImagePullPolicy != corev1.PullAlways {
								t.Errorf("imagePullPolicy = %q, want Always", container.ImagePullPolicy)
							}
							for _, name := range []string{"local", "global"} {
								if !slices.Contains(pod.ImagePullSecrets, corev1.LocalObjectReference{Name: name}) {
									t.Errorf("missing imagePullSecret %q", name)
								}
							}
						}
						args := strings.Join(container.Args, " ")
						for _, required := range []string{
							"--listen-address=:50051", "--metrics-address=:9090",
							"--injector-spiffe-id=spiffe://cluster.local/ns/" + tc.namespace + "/sa/" + tc.prefix + "atenet-egress",
							"--server-cred-bundle=/run/servicedns.podcert.ate.dev/credential-bundle.pem",
							"--client-ca-file=/run/podidentity.podcert.ate.dev/trust-bundle.pem",
						} {
							if tc.tool == "kubectl" && strings.HasPrefix(required, "--injector-spiffe-id=") {
								continue
							}
							if !strings.Contains(args, required) {
								t.Errorf("provider missing %s", required)
							}
						}
						if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Port.StrVal != "metrics" {
							t.Fatal("missing dedicated readiness probe")
						}
					}
				case "Service":
					if doc.Metadata.Name != tc.prefix+"k8s-credential-provider" {
						continue
					}
					for _, port := range doc.Spec.Ports {
						if port.Port == 50051 && port.TargetPort.StrVal == "grpc" {
							portFound = true
						}
					}
				case "ConfigMap":
					if !strings.HasPrefix(doc.Metadata.Name, tc.prefix+"k8s-credential-provider-namespace-policy") {
						continue
					}
					policyFound = true
					var policy namespacePolicyFile
					if err := yaml.UnmarshalStrict([]byte(doc.Data["namespace-policy.yaml"]), &policy); err != nil {
						t.Fatal(err)
					}
					auth, err := newNamespaceAuthorizer(policy)
					if err != nil {
						t.Fatal(err)
					}
					if auth.Allowed("team-a", "ns1") != (tc.name == "custom release") {
						t.Fatal("unexpected namespace policy")
					}
				case "ClusterRole":
					if !strings.Contains(doc.Metadata.Name, "k8s-credential-provider") && doc.Metadata.Name != tc.prefix+"ate-api-server-role" {
						continue
					}
					for _, rule := range doc.Rules {
						for _, resource := range rule.Resources {
							if resource == "secrets" || resource == "*" {
								t.Fatal("provider grants cluster-wide Secret access")
							}
						}
					}
				}
			}
			if providerFound != tc.enabled || portFound != tc.enabled || policyFound != tc.enabled || accountFound != tc.enabled {
				t.Fatalf("provider=%v port=%v policy=%v account=%v, enabled=%v", providerFound, portFound, policyFound, accountFound, tc.enabled)
			}
		})
	}
}

func TestAgentgatewayCredentialConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, tool, host, roots string
		args                    []string
		enabled                 bool
	}{
		{name: "disabled", tool: "helm", args: []string{"template", "substrate", "../../../charts/substrate", "-n", "ate-system"}},
		{name: "helm", tool: "helm", host: "test-k8s-credential-provider.custom.svc:50051", roots: "/run/servicedns.podcert.ate.dev/trust-bundle.pem", enabled: true,
			args: []string{"template", "test", "../../../charts/substrate", "-n", "custom", "--set", "credentialProvider.enabled=true"}},
		{name: "kustomize", tool: "kubectl", host: "k8s-credential-provider.ate-system.svc:50051", roots: "/run/servicedns-ca/trust-bundle.pem", enabled: true,
			args: []string{"kustomize", "--load-restrictor=LoadRestrictionsNone", "../../../manifests/ate-install/agentgateway-egress-mitm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.tool); err != nil {
				t.Skipf("%s is not installed", tc.tool)
			}
			data, err := exec.CommandContext(t.Context(), tc.tool, tc.args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, data)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			providers, mitmMounts := 0, 0
			for {
				var doc struct {
					Kind string
					Data map[string]string
					Spec struct{ Template corev1.PodTemplateSpec }
				}
				if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if doc.Kind == "Deployment" {
					for _, container := range doc.Spec.Template.Spec.Containers {
						if container.Name != "agentgateway" {
							continue
						}
						for _, mount := range container.VolumeMounts {
							if mount.MountPath == "/run/egress-mitm" {
								mitmMounts++
							}
						}
					}
				}
				if doc.Kind != "ConfigMap" {
					continue
				}
				var config struct {
					Binds []struct {
						Listeners []struct {
							Protocol string
							TLS      struct{ Mode, Cert, Key string }
							Routes   []struct {
								Policies struct {
									SubstrateEgress struct {
										CredentialProviders []struct {
											URIAuthority string `json:"uriAuthority"`
											Target       struct {
												Host     string
												Policies struct {
													BackendTLS struct{ Cert, Key, Root string }
												}
											}
										}
									}
								}
							}
						}
					}
				}
				if err := yaml.Unmarshal([]byte(doc.Data["config.yaml"]), &config); err != nil {
					t.Fatal(err)
				}
				for _, bind := range config.Binds {
					for _, listener := range bind.Listeners {
						for _, route := range listener.Routes {
							for _, provider := range route.Policies.SubstrateEgress.CredentialProviders {
								providers++
								if listener.Protocol != "HTTPS" || listener.TLS.Mode != "dynamicCa" {
									t.Fatal("credentials enabled outside TLS interception")
								}
								if listener.TLS.Cert != "/run/egress-mitm/tls.crt" || listener.TLS.Key != "/run/egress-mitm/tls.key" {
									t.Fatal("incorrect MITM certificate paths")
								}
								if provider.URIAuthority != "kubernetes.io" || provider.Target.Host != tc.host {
									t.Fatalf("incorrect provider: %+v", provider)
								}
								tls := provider.Target.Policies.BackendTLS
								if tls.Root != tc.roots || tls.Cert != "/run/podidentity.podcert.ate.dev/credential-bundle.pem" || tls.Key != tls.Cert {
									t.Fatalf("incorrect provider mTLS: %+v", tls)
								}
							}
						}
					}
				}
			}
			want := 0
			if tc.enabled {
				want = 1
			}
			if providers != want || mitmMounts != want {
				t.Fatalf("providers=%d MITM mounts=%d, want %d", providers, mitmMounts, want)
			}
		})
	}
}
