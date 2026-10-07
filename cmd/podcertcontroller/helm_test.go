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

	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestHelmControllerInstallation(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "certificates", "../../charts/substrate-podcert", "-n", "certificate-system", "--set", "global.imageRegistry=mirror.example", "--set", "image.tag=test", "--set", "postgresClientNamespace=application-system").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	found := false
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var resource struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta     `json:"metadata"`
			Spec            appsv1.DeploymentSpec `json:"spec"`
			Subjects        []rbacv1.Subject      `json:"subjects"`
		}
		if err := yaml.Unmarshal([]byte(doc), &resource); err != nil {
			t.Fatal(err)
		}
		if resource.Metadata.Namespace != "" && resource.Metadata.Namespace != "certificate-system" {
			t.Errorf("%s/%s has namespace %q", resource.Kind, resource.Metadata.Name, resource.Metadata.Namespace)
		}
		for _, subject := range resource.Subjects {
			if subject.Kind == "ServiceAccount" && subject.Namespace != "certificate-system" {
				t.Errorf("%s references service account in %q", resource.Kind, subject.Namespace)
			}
		}
		if resource.Kind != "Deployment" {
			continue
		}
		found = true
		pod := resource.Spec.Template.Spec
		if len(pod.Containers) != 1 || pod.Containers[0].Image != "mirror.example/kagent-dev/substrate/podcertcontroller:test" {
			t.Fatalf("unexpected controller containers: %+v", pod.Containers)
		}
		if !slices.Contains(pod.Containers[0].Args, "--postgres-client-namespace=application-system") {
			t.Fatal("controller is missing the configured PostgreSQL client namespace")
		}
		for _, arg := range pod.Containers[0].Args {
			name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			if pflag.Lookup(name) == nil {
				t.Errorf("unknown controller flag %q", name)
			}
		}
		if len(pod.Volumes) != 1 || pod.Volumes[0].Projected == nil {
			t.Fatalf("controller must bootstrap from CA-pool Secrets: %+v", pod.Volumes)
		}
		sources := pod.Volumes[0].Projected.Sources
		if len(sources) != 2 {
			t.Fatalf("got %d CA-pool sources", len(sources))
		}
		for _, source := range sources {
			if source.Secret == nil {
				t.Fatalf("controller depends on non-Secret projection: %+v", source)
			}
		}
	}
	if !found {
		t.Fatal("missing controller deployment")
	}
}

func TestApplicationChartDoesNotInstallCertificateController(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "substrate", "../../charts/substrate").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var resource struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &resource); err != nil {
			t.Fatal(err)
		}
		if resource.Metadata.Name == "podcertificate-controller" || resource.Metadata.Namespace == "podcertificate-controller-system" {
			t.Fatalf("application chart owns certificate-controller resource %s/%s", resource.Kind, resource.Metadata.Name)
		}
	}
}
