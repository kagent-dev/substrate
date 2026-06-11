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

package ategcs

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// SendLocalDirectoryToGCSWithTarZstd uploads localDir as a single tar.zstd
// object. The storage URL may be backed by any ObjectStorage implementation.
func SendLocalDirectoryToGCSWithTarZstd(ctx context.Context, client ObjectStorage, storageURL, localDir string) (err error) {
	ctx, span := tracer.Start(ctx, "sendLocalDirectoryToGCSWithTarZstd")
	defer span.End()

	tmpFile, err := os.CreateTemp("", "substrate-upload-dir-*.tar.zstd")
	if err != nil {
		return fmt.Errorf("while creating temp archive: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	zw, err := zstd.NewWriter(tmpFile)
	if err != nil {
		return fmt.Errorf("while creating zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	if err := writeTarDirectory(tw, localDir); err != nil {
		tw.Close()
		zw.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		zw.Close()
		return fmt.Errorf("while closing tar writer: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("while closing zstd writer: %w", err)
	}
	if _, err := tmpFile.Seek(0, 0); err != nil {
		return fmt.Errorf("while seeking temp archive: %w", err)
	}

	bucket, object, err := parseGCSURL(storageURL)
	if err != nil {
		return fmt.Errorf("while parsing URL: %w", err)
	}
	if err := client.PutObject(ctx, bucket, object, tmpFile); err != nil {
		return fmt.Errorf("while putting object: %w", err)
	}
	return nil
}

// FetchLocalDirectoryFromGCSWithTarZstd downloads a tar.zstd object into
// localDir, replacing any existing contents.
func FetchLocalDirectoryFromGCSWithTarZstd(ctx context.Context, client ObjectStorage, storageURL, localDir string) (err error) {
	ctx, span := tracer.Start(ctx, "fetchLocalDirectoryFromGCSWithTarZstd")
	defer span.End()

	bucket, object, err := parseGCSURL(storageURL)
	if err != nil {
		return fmt.Errorf("while parsing URL: %w", err)
	}
	rc, err := client.GetObject(ctx, bucket, object)
	if err != nil {
		return fmt.Errorf("while getting object: %w", err)
	}
	defer rc.Close()

	zr, err := zstd.NewReader(rc, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return fmt.Errorf("while creating zstd reader: %w", err)
	}
	defer zr.Close()

	if err := os.RemoveAll(localDir); err != nil {
		return fmt.Errorf("while clearing %q: %w", localDir, err)
	}
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return fmt.Errorf("while creating %q: %w", localDir, err)
	}

	if err := extractTarDirectory(tar.NewReader(zr), localDir); err != nil {
		return err
	}
	return nil
}

func writeTarDirectory(tw *tar.Writer, localDir string) error {
	return filepath.WalkDir(localDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == localDir {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("while stating %q: %w", path, err)
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("while relativizing %q: %w", path, err)
		}
		rel = filepath.ToSlash(rel)

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("while creating tar header for %q: %w", path, err)
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("while writing tar header for %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("while opening %q: %w", path, err)
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("while adding %q to archive: %w", path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("while closing %q: %w", path, closeErr)
		}
		return nil
	})
}

func extractTarDirectory(tr *tar.Reader, localDir string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("while reading tar entry: %w", err)
		}

		name, skip, err := validateArchiveName(hdr.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry: %w", err)
		}
		if skip {
			continue
		}
		target := filepath.Join(localDir, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode).Perm()); err != nil {
				return fmt.Errorf("while creating directory %q: %w", name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("while creating parent for %q: %w", name, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode).Perm())
			if err != nil {
				return fmt.Errorf("while creating file %q: %w", name, err)
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("while extracting file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("while closing file %q: %w", name, closeErr)
			}
		default:
			return fmt.Errorf("unsupported tar entry type %d for %q", hdr.Typeflag, name)
		}
	}
}

func validateArchiveName(name string) (cleaned string, skip bool, err error) {
	if name == "" {
		return "", true, nil
	}
	cleaned = filepath.Clean(name)
	if cleaned == "." {
		return "", true, nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", true, nil
	}
	if !filepath.IsLocal(cleaned) {
		return "", false, fmt.Errorf("not a local path: %q", name)
	}
	return filepath.ToSlash(cleaned), false, nil
}
