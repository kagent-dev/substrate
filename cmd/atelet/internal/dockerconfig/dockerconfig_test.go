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

package dockerconfig

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

func writeConfig(t *testing.T, dir, file, body string) string {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func repo(t *testing.T, ref string) name.Repository {
	t.Helper()
	r, err := name.NewRepository(ref)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func authorization(t *testing.T, a authn.Authenticator) authn.AuthConfig {
	t.Helper()
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	return *cfg
}

func TestNewRejectsMissingOrInvalidFiles(t *testing.T) {
	if _, err := New(); err == nil {
		t.Error("New() with no paths succeeded")
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("New() with a missing file succeeded")
	}
	bad := writeConfig(t, t.TempDir(), "bad.json", `{"auths": `)
	if _, err := New(bad); err == nil {
		t.Error("New() with an unparseable file succeeded")
	}
}

func TestCandidatesResolvesLikeTheDockerCLI(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("svc:tok"))
	path := writeConfig(t, dir, ".dockerconfigjson", `{"auths":{
		"https://registry.example.com":{"auth":"`+auth+`","email":"","username":"svc"},
		"exact.example.com/team/app":{"username":"exact","password":"p"},
		"https://index.docker.io/v1/":{"username":"hub","password":"h"}
	}}`)
	kc, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		image string
		want  *authn.AuthConfig // nil: no credentials
	}{
		{"registry.example.com/team/app", &authn.AuthConfig{Username: "svc", Password: "tok"}},
		{"registry.example.com/other", &authn.AuthConfig{Username: "svc", Password: "tok"}},
		{"exact.example.com/team/app", &authn.AuthConfig{Username: "exact", Password: "p"}},
		// A key with a path still matches by host, as it does for the Docker CLI.
		{"exact.example.com/team/other", &authn.AuthConfig{Username: "exact", Password: "p"}},
		{"busybox", &authn.AuthConfig{Username: "hub", Password: "h"}},
		{"index.docker.io/library/busybox", &authn.AuthConfig{Username: "hub", Password: "h"}},
		{"ghcr.io/org/image", nil},
	} {
		got, err := kc.Candidates(context.Background(), repo(t, tc.image))
		if err != nil {
			t.Fatalf("%s: %v", tc.image, err)
		}
		if tc.want == nil {
			if len(got) != 0 {
				t.Errorf("%s: got %d candidates, want none", tc.image, len(got))
			}
			a, err := kc.Resolve(repo(t, tc.image))
			if err != nil || a != authn.Anonymous {
				t.Errorf("%s: Resolve = %v, %v; want Anonymous", tc.image, a, err)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d candidates, want 1", tc.image, len(got))
		}
		if cfg := authorization(t, got[0]); cfg.Username != tc.want.Username || cfg.Password != tc.want.Password {
			t.Errorf("%s: got %s/%q, want %s/%q", tc.image, cfg.Username, cfg.Password, tc.want.Username, tc.want.Password)
		}
	}
}

func TestCandidatesAcrossFilesKeepsOrderAndDedupes(t *testing.T) {
	dir := t.TempDir()
	first := writeConfig(t, dir, "first.json", `{"auths":{"registry.example.com":{"username":"a","password":"1"}}}`)
	second := writeConfig(t, dir, "second.json", `{"auths":{"registry.example.com":{"username":"b","password":"2"}}}`)
	third := writeConfig(t, dir, "third.json", `{"auths":{"registry.example.com":{"username":"a","password":"1"}}}`)
	kc, err := New(first, second, third)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kc.Candidates(context.Background(), repo(t, "registry.example.com/app"))
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, a := range got {
		users = append(users, authorization(t, a).Username)
	}
	if len(users) != 2 || users[0] != "a" || users[1] != "b" {
		t.Errorf("candidates = %v, want [a b]", users)
	}
}

func TestCandidatesRereadsFilesAndSkipsBrokenOnes(t *testing.T) {
	dir := t.TempDir()
	rotating := writeConfig(t, dir, "rotating.json", `{"auths":{"registry.example.com":{"username":"old","password":"1"}}}`)
	stable := writeConfig(t, dir, "stable.json", `{"auths":{"registry.example.com":{"username":"stable","password":"s"}}}`)
	kc, err := New(rotating, stable)
	if err != nil {
		t.Fatal(err)
	}
	target := repo(t, "registry.example.com/app")

	// A rotated Secret is picked up on the next pull, no restart.
	writeConfig(t, dir, "rotating.json", `{"auths":{"registry.example.com":{"username":"new","password":"2"}}}`)
	got, err := kc.Candidates(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || authorization(t, got[0]).Username != "new" {
		t.Errorf("after rotation: got %d candidates, first %q; want 2 with first \"new\"", len(got), authorization(t, got[0]).Username)
	}

	// A file that disappears or breaks is skipped; the others still answer.
	if err := os.Remove(rotating); err != nil {
		t.Fatal(err)
	}
	got, err = kc.Candidates(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || authorization(t, got[0]).Username != "stable" {
		t.Errorf("after removal: got %d candidates; want only \"stable\"", len(got))
	}
}
