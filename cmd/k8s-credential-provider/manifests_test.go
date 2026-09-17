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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestSidecarManifests(t *testing.T) {
	for _, tc := range []struct {
		name, tool, namespace, prefix string
		args                          []string
		enabled                       bool
	}{
		{name: "disabled", tool: "helm", namespace: "ate-system", args: []string{"template", "substrate", "../../charts/substrate", "-n", "ate-system"}},
		{name: "custom release", tool: "helm", namespace: "custom", prefix: "test-", enabled: true,
			args: []string{"template", "test", "../../charts/substrate", "-n", "custom", "--set", "ateApi.credentialProvider.enabled=true", "--set", "ateApi.credentialProvider.namespacePolicies[0].atespace=team-a", "--set", "ateApi.credentialProvider.namespacePolicies[0].allowedNamespaces[0]=ns1"}},
		{name: "kustomize", tool: "kubectl", namespace: "ate-system", enabled: true,
			args: []string{"kustomize", "--load-restrictor=LoadRestrictionsNone", "../../manifests/ate-install/kubernetes-credentials"}},
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
			var sidecarFound, portFound, policyFound bool
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
				case "Deployment":
					if doc.Metadata.Name != tc.prefix+"ate-api-server" {
						continue
					}
					pod := doc.Spec.Template.Spec
					if pod.ServiceAccountName != tc.prefix+"ate-api-server" {
						t.Fatalf("unexpected ServiceAccount %q", pod.ServiceAccountName)
					}
					for _, container := range pod.Containers {
						if container.Name != "credential-provider" {
							continue
						}
						sidecarFound = true
						args := strings.Join(container.Args, " ")
						for _, required := range []string{
							"--listen-address=:50051", "--metrics-address=:9091",
							"--injector-spiffe-id=spiffe://cluster.local/ns/" + tc.namespace + "/sa/" + tc.prefix + "atenet-egress",
							"--server-cred-bundle=/run/servicedns.podcert.ate.dev/credential-bundle.pem",
							"--client-ca-file=/run/podidentity.podcert.ate.dev/trust-bundle.pem",
						} {
							if !strings.Contains(args, required) {
								t.Errorf("sidecar missing %s", required)
							}
						}
						if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Port.StrVal != "cred-health" {
							t.Fatal("missing dedicated readiness probe")
						}
						for _, port := range container.Ports {
							if port.ContainerPort == 443 || port.ContainerPort == 9090 {
								t.Fatalf("sidecar conflicts with ateapi on %d", port.ContainerPort)
							}
						}
					}
				case "Service":
					if doc.Metadata.Name != tc.prefix+"api" {
						continue
					}
					for _, port := range doc.Spec.Ports {
						if port.Port == 50051 && port.TargetPort.StrVal == "credentials" {
							portFound = true
						}
					}
				case "ConfigMap":
					if !strings.HasPrefix(doc.Metadata.Name, tc.prefix+"credential-provider-policy") {
						continue
					}
					policyFound = true
					var policy namespacePolicyFile
					if err := yaml.UnmarshalStrict([]byte(doc.Data["policy.yaml"]), &policy); err != nil {
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
					if doc.Metadata.Name != tc.prefix+"ate-api-server-role" && doc.Metadata.Name != "ate-api-server" {
						continue
					}
					for _, rule := range doc.Rules {
						for _, resource := range rule.Resources {
							if resource == "secrets" || resource == "*" {
								t.Fatal("sidecar grants cluster-wide Secret access")
							}
						}
					}
				}
			}
			if sidecarFound != tc.enabled || portFound != tc.enabled || policyFound != tc.enabled {
				t.Fatalf("sidecar=%v port=%v policy=%v, enabled=%v", sidecarFound, portFound, policyFound, tc.enabled)
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
		{name: "disabled", tool: "helm", args: []string{"template", "substrate", "../../charts/substrate", "-n", "ate-system"}},
		{name: "helm", tool: "helm", host: "test-api.custom.svc:50051", roots: "/run/servicedns.podcert.ate.dev/trust-bundle.pem", enabled: true,
			args: []string{"template", "test", "../../charts/substrate", "-n", "custom", "--set", "ateApi.credentialProvider.enabled=true"}},
		{name: "kustomize", tool: "kubectl", host: "api.ate-system.svc:50051", roots: "/run/servicedns-ca/trust-bundle.pem", enabled: true,
			args: []string{"kustomize", "--load-restrictor=LoadRestrictionsNone", "../../manifests/ate-install/agentgateway-egress-mitm"}},
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
