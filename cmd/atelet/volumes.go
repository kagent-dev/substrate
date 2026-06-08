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
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var volumeDir = ateompath.VolumeDir

type containerBindMount struct {
	source   string
	dest     string
	readOnly bool
}

func materializeVolumes(actorTemplateNamespace, actorTemplateName, actorID string, volumes []*ateletpb.ResolvedVolume) error {
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		name := strings.TrimSpace(vol.GetName())
		if name == "" {
			return fmt.Errorf("resolved volume name is required")
		}
		root := volumeDir(actorTemplateNamespace, actorTemplateName, actorID, name)
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("clear volume dir %q: %w", root, err)
		}
		if len(vol.GetFiles()) == 0 {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return fmt.Errorf("mkdir empty volume %q: %w", name, err)
			}
			continue
		}
		for _, file := range vol.GetFiles() {
			rel := filepath.FromSlash(strings.TrimSpace(file.GetRelativePath()))
			if rel == "" || rel == "." {
				return fmt.Errorf("volume %q file relative path is required", name)
			}
			if strings.Contains(rel, "..") {
				return fmt.Errorf("volume %q file path %q escapes volume root", name, rel)
			}
			target := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent for volume %q file %q: %w", name, rel, err)
			}
			mode := os.FileMode(file.GetMode())
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(target, file.GetContent(), mode); err != nil {
				return fmt.Errorf("write volume %q file %q: %w", name, rel, err)
			}
		}
	}
	return nil
}

func containerBindMounts(
	actorTemplateNamespace, actorTemplateName, actorID string,
	mounts []*ateletpb.VolumeMount,
) ([]containerBindMount, error) {
	out := make([]containerBindMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount == nil {
			continue
		}
		name := strings.TrimSpace(mount.GetName())
		if name == "" {
			return nil, fmt.Errorf("volume mount name is required")
		}
		if err := validateContainerMountPath(mount.GetMountPath()); err != nil {
			return nil, err
		}
		sourceRoot := volumeDir(actorTemplateNamespace, actorTemplateName, actorID, name)
		source := sourceRoot
		if sub := strings.TrimSpace(mount.GetSubPath()); sub != "" {
			rel := filepath.FromSlash(sub)
			if strings.Contains(rel, "..") {
				return nil, fmt.Errorf("volume mount subPath %q escapes volume root", sub)
			}
			source = filepath.Join(sourceRoot, rel)
		}
		out = append(out, containerBindMount{
			source:   source,
			dest:     mount.GetMountPath(),
			readOnly: mount.GetReadOnly(),
		})
	}
	return out, nil
}

func validateContainerMountPath(mountPath string) error {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		return fmt.Errorf("volume mount mountPath is required")
	}
	if !filepath.IsAbs(mountPath) {
		return fmt.Errorf("volume mount mountPath %q must be absolute", mountPath)
	}
	return nil
}

func bindMountsToOCISpec(mounts []containerBindMount) []specs.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]specs.Mount, 0, len(mounts))
	for _, mount := range mounts {
		options := []string{"bind"}
		if mount.readOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}
		out = append(out, specs.Mount{
			Destination: mount.dest,
			Type:        "bind",
			Source:      mount.source,
			Options:     options,
		})
	}
	return out
}
