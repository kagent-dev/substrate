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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type memoryStorage struct {
	objects map[string][]byte
}

func (m *memoryStorage) GetObject(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
	data, ok := m.objects[bucket+"/"+object]
	if !ok {
		return nil, fmt.Errorf("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memoryStorage) PutObject(ctx context.Context, bucket, object string, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.objects[bucket+"/"+object] = data
	return nil
}

func TestDirectoryTarZstdRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "checkpoint.img"), []byte("checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "pages.img"), []byte("pages"), 0o640); err != nil {
		t.Fatal(err)
	}

	store := &memoryStorage{objects: map[string][]byte{}}
	uri := "gs://bucket/templates/default/counter/cloud-hypervisor/golden/app.tar.zstd"
	if err := SendLocalDirectoryToGCSWithTarZstd(ctx, store, uri, src); err != nil {
		t.Fatalf("upload directory: %v", err)
	}

	dst := t.TempDir()
	if err := FetchLocalDirectoryFromGCSWithTarZstd(ctx, store, uri, dst); err != nil {
		t.Fatalf("download directory: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "checkpoint.img"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "checkpoint" {
		t.Errorf("checkpoint.img = %q, want checkpoint", got)
	}
	got, err = os.ReadFile(filepath.Join(dst, "nested", "pages.img"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pages" {
		t.Errorf("pages.img = %q, want pages", got)
	}
}

func TestValidateArchiveNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../x", "a/../../x"} {
		if _, _, err := validateArchiveName(name); err == nil {
			t.Errorf("validateArchiveName(%q) succeeded, want error", name)
		}
	}
}
