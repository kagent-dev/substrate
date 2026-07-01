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

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const PauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

const (
	SandboxClassGvisor  = "gvisor"
	SandboxClassMicroVM = "microvm"
)

func KoBuild(t testing.TB, importPath string) string {
	t.Helper()
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	cmd := exec.Command(filepath.Join(root, "hack/run-tool.sh"), "ko", "build", importPath, "--bare")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "KO_CONFIG_PATH="+root, "KO_DOCKER_REPO="+koDockerRepoForBuild(os.Getenv("KO_DOCKER_REPO")))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ko build %s failed: %v\n%s", importPath, err, string(out))
	}
	image := lastNonEmptyLine(string(out))
	if image == "" {
		t.Fatalf("ko build %s returned empty image", importPath)
	}
	return image
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func koDockerRepoForBuild(repo string) string {
	if repo == "localhost:5001" {
		return "localhost:5001/substrate"
	}
	return repo
}

func EnsureGvisorSandboxConfig(t testing.TB, ctx context.Context, clients *Clients) {
	t.Helper()
	_, err := clients.SubstrateAPI.CreateSandboxConfig(ctx, &ateapipb.CreateSandboxConfigRequest{
		SandboxConfig: &ateapipb.SandboxConfig{
			Name: "gvisor-default",
			Spec: &ateapipb.SandboxConfigSpec{
				SandboxClass: SandboxClassGvisor,
				Default:      true,
				Assets: map[string]*ateapipb.SandboxAssetFiles{
					"amd64": {Files: map[string]*ateapipb.AssetFile{
						"runsc": {
							Url:    "gs://gvisor/releases/release/20260622/x86_64/runsc",
							Sha256: "f18a948bf9c8bbb54eb998549a3a8d719a1c7de2efbe8fdd2ff0ee5fecd06f19",
						},
					}},
					"arm64": {Files: map[string]*ateapipb.AssetFile{
						"runsc": {
							Url:    "gs://gvisor/releases/release/20260622/aarch64/runsc",
							Sha256: "62eee121f8c188e347c428acc96f111568ede3be37b906046b6f28bbe2cc40c0",
						},
					}},
				},
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateSandboxConfig(gvisor-default): %v", err)
	}
}

func EnsureMicroVMSandboxConfig(t testing.TB, ctx context.Context, clients *Clients, name, bucket, virtiofsdSHA256 string) {
	t.Helper()
	if name == "" {
		t.Fatalf("microVM SandboxConfig name must not be empty")
	}
	if bucket == "" {
		t.Fatalf("microVM SandboxConfig requires BUCKET_NAME")
	}
	if virtiofsdSHA256 == "" {
		t.Fatalf("microVM SandboxConfig requires VIRTIOFSD_SHA256")
	}
	_, err := clients.SubstrateAPI.CreateSandboxConfig(ctx, &ateapipb.CreateSandboxConfigRequest{
		SandboxConfig: &ateapipb.SandboxConfig{
			Name: name,
			Spec: &ateapipb.SandboxConfigSpec{
				SandboxClass: SandboxClassMicroVM,
				Assets: map[string]*ateapipb.SandboxAssetFiles{
					"arm64": {Files: microVMAssetFiles(bucket, virtiofsdSHA256, map[string]string{
						"cloud-hypervisor": "bf004ddc1a148f47caa87ac49a783b8dbd6bf9bc27abe522ed197df7b982d3b1",
						"kata-kernel":      "f437320bab94f19105d12b932aa29735f0d54d2588218872254367f312c1027c",
						"kata-image":       "31ffb41177571c5654d3a28a2728eaac9d6d3daed90bb993f64e0b4b3ca6b235",
						"kata-config":      "8a09a40543a527dbdc3ff26d229bae0de9aebb655475c28d7e5482dbedefa030",
					})},
					"amd64": {Files: microVMAssetFiles(bucket, virtiofsdSHA256, map[string]string{
						"cloud-hypervisor": "829af01ff075bb96c4f183905134c453a88d68cbabdc6b87df21098842581ee9",
						"kata-kernel":      "43701715ae2885f936bbe5c66a2de7c14dc51de7d19412d04833e4bbcf205bd0",
						"kata-image":       "e9548ff64f51c120791d3a2d1a81ebfd275df2bf0737368bd3e6381a6e967855",
						"kata-config":      "8cce580e5abf78c05c8e9b929c24a524b9a81fc47be4e2e4f38dcae5ef052be6",
					})},
				},
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateSandboxConfig(%s): %v", name, err)
	}
}

func microVMAssetFiles(bucket, virtiofsdSHA256 string, sha map[string]string) map[string]*ateapipb.AssetFile {
	files := map[string]*ateapipb.AssetFile{
		"cloud-hypervisor": {Url: fmt.Sprintf("gs://%s/kata-assets/cloud-hypervisor", bucket), Sha256: sha["cloud-hypervisor"]},
		"virtiofsd":        {Url: fmt.Sprintf("gs://%s/kata-assets/virtiofsd", bucket), Sha256: virtiofsdSHA256},
		"kata-kernel":      {Url: fmt.Sprintf("gs://%s/kata-assets/vmlinux", bucket), Sha256: sha["kata-kernel"]},
		"kata-image":       {Url: fmt.Sprintf("gs://%s/kata-assets/rootfs.img", bucket), Sha256: sha["kata-image"]},
		"kata-config":      {Url: fmt.Sprintf("gs://%s/kata-assets/configuration-clh.toml", bucket), Sha256: sha["kata-config"]},
	}
	return files
}
