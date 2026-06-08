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
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func withTempVolumeDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := volumeDir
	volumeDir = func(_, _, _, volumeName string) string {
		return filepath.Join(root, volumeName)
	}
	t.Cleanup(func() { volumeDir = old })
	return root
}

func TestMaterializeVolumesWritesFiles(t *testing.T) {
	t.Parallel()
	root := withTempVolumeDir(t)
	volumes := []*ateletpb.ResolvedVolume{
		{
			Name: "skills",
			Files: []*ateletpb.VolumeFile{
				{RelativePath: "SKILL.md", Content: []byte("# hello"), Mode: 0o644},
				{RelativePath: "nested/tool.yaml", Content: []byte("tool: true"), Mode: 0o600},
			},
		},
	}
	if err := materializeVolumes("ns", "tmpl", "actor-1", volumes); err != nil {
		t.Fatalf("materializeVolumes: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "skills", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if string(got) != "# hello" {
		t.Fatalf("got %q", string(got))
	}
}

func TestMaterializeVolumesCreatesEmptyDir(t *testing.T) {
	t.Parallel()
	root := withTempVolumeDir(t)
	if err := materializeVolumes("ns", "tmpl", "actor-1", []*ateletpb.ResolvedVolume{
		{Name: "scratch"},
	}); err != nil {
		t.Fatalf("materializeVolumes: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatalf("stat emptyDir volume: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected directory, got %v", info.Mode())
	}
}

func TestContainerBindMountsUsesSubPath(t *testing.T) {
	t.Parallel()
	root := withTempVolumeDir(t)
	mounts, err := containerBindMounts("ns", "tmpl", "actor-1", []*ateletpb.VolumeMount{
		{Name: "cfg", MountPath: "/etc/app", SubPath: "app/config.yaml", ReadOnly: true},
	})
	if err != nil {
		t.Fatalf("containerBindMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected one mount, got %d", len(mounts))
	}
	wantSource := filepath.Join(root, "cfg", "app", "config.yaml")
	if mounts[0].source != wantSource {
		t.Fatalf("source = %q, want %q", mounts[0].source, wantSource)
	}
	if mounts[0].dest != "/etc/app" || !mounts[0].readOnly {
		t.Fatalf("unexpected mount: %+v", mounts[0])
	}
}

func TestBindMountsToOCISpec(t *testing.T) {
	t.Parallel()
	got := bindMountsToOCISpec([]containerBindMount{
		{source: "/host/data", dest: "/data", readOnly: true},
		{source: "/host/scratch", dest: "/tmp", readOnly: false},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(got))
	}
	if got[0].Type != "bind" || got[0].Destination != "/data" {
		t.Fatalf("unexpected first mount: %+v", got[0])
	}
	if !contains(got[0].Options, "ro") {
		t.Fatalf("expected read-only mount, got %v", got[0].Options)
	}
	if contains(got[1].Options, "ro") {
		t.Fatalf("expected read-write mount, got %v", got[1].Options)
	}
}

func contains(opts []string, want string) bool {
	for _, opt := range opts {
		if opt == want {
			return true
		}
	}
	return false
}

func TestBindMountsToOCISpecNil(t *testing.T) {
	t.Parallel()
	if got := bindMountsToOCISpec(nil); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
	var empty []specs.Mount
	if got := bindMountsToOCISpec([]containerBindMount{}); got != nil && len(got) != len(empty) {
		t.Fatalf("expected empty slice, got %#v", got)
	}
}
