// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func releaseEnv(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"gh", "helm"} {
		if err := os.Symlink(filepath.Join(root, "hack/release/testdata", name+".sh"), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(bin, "helm.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELEASE_TEST_LOG", log)
	t.Setenv("RELEASE_TEST_STATE", "missing")
	t.Setenv("RELEASE_TEST_FAIL", "")
	t.Setenv("VERSION", "v1.0.0")
	t.Setenv("GITHUB_REPOSITORY", "kagent-dev/substrate")
	t.Setenv("GITHUB_ACTOR", "release-test")
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("KO_DOCKER_REPO", "ghcr.io/kagent-dev/substrate")
	return root, log
}

func TestPublishCharts(t *testing.T) {
	for _, tc := range []struct {
		name, state, fail, version string
		wantErr, wantPush          bool
	}{
		{name: "new release", state: "missing", version: "v1.0.0", wantPush: true},
		{name: "draft rerun", state: "draft", version: "v1.0.0", wantPush: true},
		{name: "prerelease", state: "missing", version: "v1.0.0-rc.1", wantPush: true},
		{name: "published", state: "published", version: "v1.0.0", wantErr: true},
		{name: "API failure", state: "unavailable", version: "v1.0.0", wantErr: true},
		{name: "package failure", state: "missing", fail: "package", version: "v1.0.0", wantErr: true},
		{name: "login failure", state: "missing", fail: "registry", version: "v1.0.0", wantErr: true},
		{name: "push failure", state: "missing", fail: "push", version: "v1.0.0", wantErr: true, wantPush: true},
		{name: "invalid version", state: "missing", version: "v1.0.0,latest", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, log := releaseEnv(t)
			t.Setenv("VERSION", tc.version)
			t.Setenv("RELEASE_TEST_STATE", tc.state)
			t.Setenv("RELEASE_TEST_FAIL", tc.fail)
			cmd := exec.Command("bash", filepath.Join(root, ".github/scripts/release/publish-charts.sh"))
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("publish charts: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			got := string(calls)
			if strings.Contains(got, "push ") != tc.wantPush {
				t.Fatalf("unexpected registry writes:\n%s", got)
			}
			if tc.state == "published" || tc.state == "unavailable" || tc.name == "invalid version" {
				if got != "" {
					t.Fatalf("helm ran before release validation:\n%s", got)
				}
			}
			if tc.wantErr {
				return
			}
			if strings.Count(got, "push ") != 3 || strings.Count(got, "package ") != 3 {
				t.Fatalf("incomplete chart set:\n%s", got)
			}
			for _, chart := range []string{"substrate-crds", "substrate", "substrate-podcert"} {
				want := chart + "-" + strings.TrimPrefix(tc.version, "v") + ".tgz oci://ghcr.io/kagent-dev/substrate/helm"
				if !strings.Contains(got, want) {
					t.Errorf("missing %s in:\n%s", want, got)
				}
			}
			if strings.Count(got, "--app-version "+tc.version) != 3 {
				t.Errorf("charts do not select the release images:\n%s", got)
			}
		})
	}
}

func TestDraftReleaseArtifactNotes(t *testing.T) {
	for _, tc := range []struct {
		repo, registry string
		artifacts      bool
	}{
		{repo: "agent-substrate/substrate", artifacts: true},
		{repo: "kagent-dev/substrate", registry: "ghcr.io/kagent-dev/substrate", artifacts: true},
		{repo: "kagent-dev/substrate", registry: "registry.example.com/releases", artifacts: true},
		{repo: "someone/substrate"},
	} {
		t.Run(tc.repo+"/"+tc.registry, func(t *testing.T) {
			root, _ := releaseEnv(t)
			t.Setenv("GITHUB_REPOSITORY", tc.repo)
			t.Setenv("KO_DOCKER_REPO", tc.registry)
			cmd := exec.Command("bash", filepath.Join(root, "hack/release/draft-release.sh"), "--dry-run", "v1.0.0")
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("draft notes: %v\n%s", err, out)
			}
			notes := string(out)
			if strings.Contains(notes, "## Images") != tc.artifacts || strings.Contains(notes, "## kubectl-ate") != tc.artifacts {
				t.Fatalf("incorrect artifact notes:\n%s", notes)
			}
			if strings.Contains(notes, "## Helm charts") != (tc.repo == "kagent-dev/substrate") {
				t.Fatalf("incorrect chart notes:\n%s", notes)
			}
			if tc.artifacts {
				repo := tc.registry
				if repo == "" {
					repo = "ghcr.io/" + tc.repo
				}
				if !strings.Contains(notes, "--image-repo "+repo+" --image-tag v1.0.0") {
					t.Errorf("incorrect install image repository:\n%s", notes)
				}
			}
		})
	}
}
