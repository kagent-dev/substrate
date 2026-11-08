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

// Package microvmassets assembles the microVM guest assets that the microvm
// SandboxConfig names, from the Kata and Cloud Hypervisor releases, and checks
// them against the digests the SandboxConfig pins.
package microvmassets

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"k8s.io/apimachinery/pkg/util/yaml"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
)

// Versions of the releases the assets come from. They must match the digests
// in manifests/ate-install/sandboxconfig-microvm.yaml.tmpl, and stay in step
// with cmd/ate-setup/internal/steps/microvm.go.
const (
	KataVersion            = "4.3.1"
	CloudHypervisorVersion = "v53.0"
	releaseBaseURL         = "https://github.com"

	// Prefix is where assets live in the bucket: Prefix/<arch>/<sha256>.
	Prefix = "microvm-assets"

	stampFile = ".asset-versions"
	maxBytes  = 8 << 30

	kataSharePrefix      = "opt/kata/share/kata-containers/"
	kataKernelSymlink    = "opt/kata/share/kata-containers/vmlinux.container"
	kataRootfsSymlink    = "opt/kata/share/kata-containers/kata-containers.img"
	kataLibexecVirtiofsd = "opt/kata/libexec/virtiofsd"
)

// templateFiles maps each asset key in the SandboxConfig to its file name.
var templateFiles = map[string]string{
	"cloud-hypervisor": "cloud-hypervisor",
	"virtiofsd":        "virtiofsd",
	"kata-kernel":      "vmlinux",
	"kata-image":       "rootfs.img",
}

// Files lists the asset file names.
var Files = []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"}

// Render fills in the SandboxConfig template for assets in bucket, reached
// through the object store plugin's scheme (s3 or gs).
func Render(tmpl []byte, scheme, bucket string) []byte {
	return []byte(strings.NewReplacer("${BUCKET_SCHEME}", scheme, "${ASSET_BUCKET_NAME}", bucket).Replace(string(tmpl)))
}

// Digests returns the SHA-256 of each asset file, by architecture, from the
// default version of the rendered SandboxConfig.
func Digests(rendered []byte) (map[string]map[string]string, error) {
	var sc atev1.SandboxConfig
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096).Decode(&sc); err != nil {
		return nil, fmt.Errorf("while decoding SandboxConfig: %w", err)
	}
	v, ok := sc.Spec.DefaultVersionConfig()
	if !ok {
		return nil, fmt.Errorf("SandboxConfig %s has no enabled version %q", sc.Name, sc.Spec.DefaultVersion)
	}
	out := map[string]map[string]string{}
	for arch, assets := range v.Assets {
		files := map[string]string{}
		for key, name := range templateFiles {
			a, ok := assets[key]
			if !ok || a.SHA256 == "" {
				return nil, fmt.Errorf("SandboxConfig %s arch %s has no sha256 for %s", sc.Name, arch, key)
			}
			if want := "/" + path.Join(Prefix, arch, a.SHA256); !strings.HasSuffix(a.URL, want) {
				return nil, fmt.Errorf("SandboxConfig %s arch %s: url %q doesn't end with %q", sc.Name, arch, a.URL, want)
			}
			files[name] = a.SHA256
		}
		out[arch] = files
	}
	return out, nil
}

// Ensure leaves the assets for arch in dir, verified against digests. It
// downloads them only when dir lacks a complete set for these versions.
func Ensure(ctx context.Context, dir, arch string, digests map[string]string, progress func(string)) error {
	stamp := fmt.Sprintf("arch=%s\nkata=%s\ncloud-hypervisor=%s\n", arch, KataVersion, CloudHypervisorVersion)
	if got, err := os.ReadFile(filepath.Join(dir, stampFile)); err == nil && string(got) == stamp {
		if err := verify(dir, digests); err == nil {
			return nil
		}
	}
	return assemble(ctx, dir, arch, stamp, digests, progress)
}

func verify(dir string, digests map[string]string) error {
	var bad []string
	for _, f := range Files {
		got, err := fileSHA256(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		if want := digests[f]; got != want {
			bad = append(bad, fmt.Sprintf("%s: got %s, want %s", f, got, want))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("microVM asset digest mismatch in %s: %s", dir, strings.Join(bad, "; "))
	}
	return nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// assemble writes the stamp last, so an interrupted run assembles again.
func assemble(ctx context.Context, dir, arch, stamp string, digests map[string]string, progress func(string)) error {
	var chAsset string
	switch arch {
	case "amd64":
		chAsset = "cloud-hypervisor-static"
	case "arm64":
		chAsset = "cloud-hypervisor-static-aarch64"
	default:
		return fmt.Errorf("no microVM assets for architecture %s", arch)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stampPath := filepath.Join(dir, stampFile)
	if err := os.Remove(stampPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	work, err := os.MkdirTemp(dir, ".assemble-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()

	kataURL := fmt.Sprintf("%s/kata-containers/kata-containers/releases/download/%s/kata-static-%s-%s.tar.zst", releaseBaseURL, KataVersion, KataVersion, arch)
	progress(fmt.Sprintf("downloading Kata %s for %s", KataVersion, arch))
	if err := extractKata(ctx, kataURL, work, dir); err != nil {
		return err
	}
	chURL := fmt.Sprintf("%s/cloud-hypervisor/cloud-hypervisor/releases/download/%s/%s", releaseBaseURL, CloudHypervisorVersion, chAsset)
	progress(fmt.Sprintf("downloading Cloud Hypervisor %s for %s", CloudHypervisorVersion, arch))
	body, err := get(ctx, chURL)
	if err != nil {
		return err
	}
	tmp := filepath.Join(work, "cloud-hypervisor")
	err = writeFile(tmp, 0o755, body)
	body.Close()
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "cloud-hypervisor")); err != nil {
		return err
	}
	if err := verify(dir, digests); err != nil {
		return err
	}
	return os.WriteFile(stampPath, []byte(stamp), 0o644)
}

func get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("while downloading %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("while downloading %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// extractKata pulls the guest kernel, rootfs image and virtiofsd out of the
// Kata static release into dir.
func extractKata(ctx context.Context, url, work, dir string) error {
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	links, err := untarZst(ctx, body, work)
	if err != nil {
		return fmt.Errorf("while extracting %s: %w", url, err)
	}
	for _, item := range []struct {
		archivePath, name string
		mode              fs.FileMode
	}{
		{kataKernelSymlink, "vmlinux", 0o644},
		// ateworker-microvm attaches rootfs.img over virtio-pmem with DAX, which
		// kata-containers.img is built for.
		{kataRootfsSymlink, "rootfs.img", 0o644},
		{kataLibexecVirtiofsd, "virtiofsd", 0o755},
	} {
		resolved := item.archivePath
		for range 8 {
			target, ok := links[resolved]
			if !ok {
				break
			}
			resolved = target
		}
		src := filepath.Join(work, filepath.FromSlash(resolved))
		if fi, err := os.Stat(src); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("kata archive has no regular file for %s", item.archivePath)
		}
		if err := os.Chmod(src, item.mode); err != nil {
			return err
		}
		if err := os.Rename(src, filepath.Join(dir, item.name)); err != nil {
			return err
		}
	}
	return nil
}

// untarZst writes the regular files under the Kata share directory, and
// virtiofsd, into work. It returns the archive's symlinks, cleaned.
func untarZst(ctx context.Context, r io.Reader, work string) (map[string]string, error) {
	zr, err := zstd.NewReader(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	links := map[string]string{}
	tr := tar.NewReader(zr)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return links, nil
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(hdr.Name, "./")), "/")
		if !strings.HasPrefix(name, kataSharePrefix) && name != kataLibexecVirtiofsd {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return nil, fmt.Errorf("tar entry %q escapes the extraction directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBytes {
				return nil, fmt.Errorf("kata archive exceeds %d bytes", int64(maxBytes))
			}
			if err := writeFile(filepath.Join(work, filepath.FromSlash(name)), 0o600, tr); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			if path.IsAbs(hdr.Linkname) {
				return nil, fmt.Errorf("tar symlink %q has absolute target %q", hdr.Name, hdr.Linkname)
			}
			target := path.Clean(path.Join(path.Dir(name), hdr.Linkname))
			if !filepath.IsLocal(filepath.FromSlash(target)) {
				return nil, fmt.Errorf("tar symlink %q target %q escapes the extraction directory", hdr.Name, hdr.Linkname)
			}
			links[name] = target
		}
	}
}

func writeFile(p string, mode fs.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxBytes {
		err = fmt.Errorf("%s exceeds %d bytes", p, int64(maxBytes))
	}
	return err
}
