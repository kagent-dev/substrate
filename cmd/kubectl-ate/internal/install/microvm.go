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

package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"go.ate.dev/substrate/cmd/kubectl-ate/internal/microvmassets"
	"go.ate.dev/substrate/internal/portforward"
	ateinstall "go.ate.dev/substrate/manifests/ate-install"
	"go.ate.dev/substrate/pkg/objectstorage"
)

var sandboxConfigsGVR = atev1.GroupVersion.WithResource("sandboxconfigs")

const (
	microVMSandboxConfig = "microvm"
	rustfsService        = "rustfs"
	rustfsPort           = 9000
)

// assetTarget is where the microVM assets are staged. endpoint, region and
// pathStyle come from atelet's objectStore values for an S3 store.
type assetTarget struct {
	store     ObjectStoreType
	scheme    string
	bucket    string
	endpoint  string
	region    string
	pathStyle bool
}

// microVMTarget reads the object store from the installed releases. The
// bucket is the RustFS snapshot bucket, Config.MicroVM.AssetBucket, or the one
// the microvm SandboxConfig already uses.
func (in *Installer) microVMTarget(ctx context.Context) (assetTarget, error) {
	sub, err := in.installedValues(substrateRelease)
	if err != nil {
		return assetTarget{}, err
	}
	if enabled, _ := lookup(sub, "rustfs", "enabled").(bool); enabled {
		bucket, _ := lookupString(sub, "rustfs", "bucket")
		if bucket == "" {
			return assetTarget{}, errors.New("substrate release has no rustfs.bucket")
		}
		return assetTarget{store: ObjectStoreRustFS, scheme: "s3", bucket: bucket}, nil
	}
	atelet, err := in.installedValues(ateletRelease)
	if err != nil {
		return assetTarget{}, err
	}
	t := assetTarget{bucket: in.opts.MicroVM.AssetBucket}
	switch backend, _ := lookupString(atelet, "objectStore", "backend"); backend {
	case "s3":
		t.store, t.scheme = ObjectStoreS3, "s3"
		t.endpoint, _ = lookupString(atelet, "objectStore", "endpoint")
		t.region, _ = lookupString(atelet, "objectStore", "region")
		t.pathStyle, _ = lookup(atelet, "objectStore", "pathStyle").(bool)
	case "gcs":
		t.store, t.scheme = ObjectStoreGCS, "gs"
	default:
		return assetTarget{}, fmt.Errorf("substrate-atelet release has unknown objectStore.backend %q", backend)
	}
	if t.bucket == "" {
		t.bucket, err = in.currentAssetBucket(ctx, t.scheme)
		if err != nil {
			return assetTarget{}, err
		}
	}
	if t.bucket == "" {
		return assetTarget{}, errors.New("set microvm.assetBucket to a bucket atelet can read; it must not hold snapshots")
	}
	return t, nil
}

// installedValues returns the values r runs with: its chart's defaults with the
// stored values on top.
func (in *Installer) installedValues(r release) (map[string]any, error) {
	rel, err := in.helm.get(r)
	if err != nil {
		return nil, err
	}
	if rel == nil {
		return nil, fmt.Errorf("release %s isn't installed", r.name)
	}
	return mergeValues(rel.Chart.Values, rel.Config), nil
}

// currentAssetBucket returns the bucket of the installed microvm
// SandboxConfig's first asset, or "" if there is none.
func (in *Installer) currentAssetBucket(ctx context.Context, scheme string) (string, error) {
	sc, err := in.dyn.Resource(sandboxConfigsGVR).Get(ctx, microVMSandboxConfig, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	versions, _ := lookup(sc.Object, "spec", "versions").([]any)
	for _, v := range versions {
		archs, _ := lookup(v, "assets").(map[string]any)
		for _, assets := range archs {
			files, _ := assets.(map[string]any)
			for _, a := range files {
				raw, _ := lookup(a, "url").(string)
				u, err := url.Parse(raw)
				if err == nil && u.Scheme == scheme {
					return u.Host, nil
				}
			}
		}
	}
	return "", nil
}

// stageMicroVMAssets downloads the assets for every node architecture, checks
// their digests, and uploads the ones the bucket lacks.
func (in *Installer) stageMicroVMAssets(ctx context.Context, progress func(string)) (string, error) {
	t, err := in.microVMTarget(ctx)
	if err != nil {
		return "", err
	}
	digests, err := microvmassets.Digests(microvmassets.Render(ateinstall.MicroVMSandboxConfig, t.scheme, t.bucket))
	if err != nil {
		return "", err
	}
	arches, err := in.nodeArches(ctx)
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	store, stop, err := in.assetStore(ctx, t)
	if err != nil {
		return "", err
	}
	defer stop()
	uploaded := 0
	for _, arch := range arches {
		want, ok := digests[arch]
		if !ok {
			return "", fmt.Errorf("SandboxConfig %s has no assets for node architecture %s", microVMSandboxConfig, arch)
		}
		dir := filepath.Join(cache, "kubectl-ate", "microvm-assets", arch)
		if err := microvmassets.Ensure(ctx, dir, arch, want, progress); err != nil {
			return "", err
		}
		for _, f := range microvmassets.Files {
			object := microvmassets.Prefix + "/" + arch + "/" + want[f]
			ok, err := store.exists(ctx, object)
			if err != nil {
				return "", fmt.Errorf("while checking %s://%s/%s: %w", t.scheme, t.bucket, object, err)
			}
			if ok {
				continue
			}
			progress(fmt.Sprintf("uploading %s/%s", arch, f))
			if err := uploadFile(ctx, store, object, filepath.Join(dir, f)); err != nil {
				return "", fmt.Errorf("while uploading %s to %s://%s/%s: %w", f, t.scheme, t.bucket, object, err)
			}
			uploaded++
		}
	}
	return fmt.Sprintf("%s in %s://%s/%s/, %d uploaded", strings.Join(arches, ", "), t.scheme, t.bucket, microvmassets.Prefix, uploaded), nil
}

// applyMicroVMSandboxConfig applies the microvm SandboxConfig for the staged
// assets.
func (in *Installer) applyMicroVMSandboxConfig(ctx context.Context, _ func(string)) (string, error) {
	t, err := in.microVMTarget(ctx)
	if err != nil {
		return "", err
	}
	var obj unstructured.Unstructured
	rendered := microvmassets.Render(ateinstall.MicroVMSandboxConfig, t.scheme, t.bucket)
	if err := yaml.Unmarshal(rendered, &obj.Object); err != nil {
		return "", fmt.Errorf("while decoding SandboxConfig %s: %w", microVMSandboxConfig, err)
	}
	_, err = in.dyn.Resource(sandboxConfigsGVR).Apply(ctx, obj.GetName(), &obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	if err != nil {
		return "", fmt.Errorf("while applying SandboxConfig %s: %w", obj.GetName(), err)
	}
	return fmt.Sprintf("%s://%s", t.scheme, t.bucket), nil
}

func (in *Installer) nodeArches(ctx context.Context) ([]string, error) {
	nodes, err := in.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var arches []string
	for _, n := range nodes.Items {
		if a := n.Status.NodeInfo.Architecture; !slices.Contains(arches, a) {
			arches = append(arches, a)
		}
	}
	if len(arches) == 0 {
		return nil, errors.New("cluster has no nodes")
	}
	slices.Sort(arches)
	return arches, nil
}

// assetBucket is the bucket the assets are staged in. exists checks an
// object without reading it.
type assetBucket interface {
	exists(ctx context.Context, object string) (bool, error)
	put(ctx context.Context, object string, r io.Reader) error
}

type s3Bucket struct {
	client *s3.Client
	store  objectstorage.ObjectStorage
	name   string
}

func newS3Bucket(client *s3.Client, name string) *s3Bucket {
	return &s3Bucket{client: client, store: objectstorage.NewS3Client(client), name: name}
}

func (b *s3Bucket) exists(ctx context.Context, object string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.name), Key: aws.String(object)})
	if _, ok := errors.AsType[*s3types.NotFound](err); ok {
		return false, nil
	}
	return err == nil, err
}

func (b *s3Bucket) put(ctx context.Context, object string, r io.Reader) error {
	return b.store.PutObject(ctx, b.name, object, r)
}

type gcsBucket struct {
	*storage.BucketHandle
}

func (b gcsBucket) exists(ctx context.Context, object string) (bool, error) {
	_, err := b.Object(object).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (b gcsBucket) put(ctx context.Context, object string, r io.Reader) error {
	// Canceling the writer's context aborts the upload. Closing it would
	// commit a partial object.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := b.Object(object).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		cancel()
		_ = w.Close()
		return err
	}
	return w.Close()
}

// assetStore returns the bucket for t. For RustFS it port-forwards to the
// in-cluster service with the credentials the install created. Otherwise it
// uses the caller's own cloud credentials.
func (in *Installer) assetStore(ctx context.Context, t assetTarget) (assetBucket, func(), error) {
	switch t.store {
	case ObjectStoreRustFS:
		access, secret, err := in.rustfsCredentials(ctx)
		if err != nil {
			return nil, nil, err
		}
		port, stop, err := portforward.ServicePodPortForward(ctx, in.rest, in.kube, SystemNamespace, rustfsService, rustfsPort)
		if err != nil {
			return nil, nil, fmt.Errorf("while port-forwarding to %s: %w", rustfsService, err)
		}
		client := s3.New(s3.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(fmt.Sprintf("http://127.0.0.1:%d", port)),
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider(access, secret, ""),
		})
		return newS3Bucket(client, t.bucket), stop, nil
	case ObjectStoreS3:
		var opts []func(*awsconfig.LoadOptions) error
		if t.region != "" {
			opts = append(opts, awsconfig.WithRegion(t.region))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("while loading AWS config: %w", err)
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if t.endpoint != "" {
				o.BaseEndpoint = aws.String(t.endpoint)
			}
			o.UsePathStyle = t.pathStyle
		})
		return newS3Bucket(client, t.bucket), func() {}, nil
	default:
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("while creating GCS client: %w", err)
		}
		return gcsBucket{client.Bucket(t.bucket)}, func() { client.Close() }, nil
	}
}

func uploadFile(ctx context.Context, b assetBucket, object, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return b.put(ctx, object, f)
}
