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
