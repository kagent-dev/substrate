// Copyright 2026 The Agent Substrate Authors
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

package microvmassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ateinstall "go.ate.dev/substrate/manifests/ate-install"
)

func TestDigests(t *testing.T) {
	rendered := Render(ateinstall.MicroVMSandboxConfig, "s3", "ate-snapshots")
	if strings.Contains(string(rendered), "${") {
		t.Fatalf("Render left a variable: %s", rendered)
	}
	got, err := Digests(rendered)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, f := range Files {
			if len(got[arch][f]) != 64 {
				t.Errorf("%s %s sha256 = %q", arch, f, got[arch][f])
			}
		}
	}
}

func TestEnsureUsesCache(t *testing.T) {
	dir := t.TempDir()
	digests := map[string]string{}
	for _, f := range Files {
		data := []byte("asset " + f)
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		digests[f] = hex.EncodeToString(sum[:])
	}
	stamp := fmt.Sprintf("arch=amd64\nkata=%s\ncloud-hypervisor=%s\n", KataVersion, CloudHypervisorVersion)
	if err := os.WriteFile(filepath.Join(dir, stampFile), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	// A canceled context fails any download, so success means the cache was used.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Ensure(ctx, dir, "amd64", digests, func(string) {}); err != nil {
		t.Fatalf("Ensure() with a complete cache = %v", err)
	}
	digests["vmlinux"] = strings.Repeat("0", 64)
	if err := Ensure(ctx, dir, "amd64", digests, func(string) {}); err == nil {
		t.Fatal("Ensure() with a stale cache and no network succeeded")
	}
}
