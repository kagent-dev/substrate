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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/kustomize/api/krusty"
	kusttypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
	"sigs.k8s.io/yaml"

	ateinstall "go.ate.dev/substrate/manifests/ate-install"
	"go.ate.dev/substrate/pkg/postgressetup"
)

const (
	fieldManager = "kubectl-ate"

	postgresStatefulSet = "postgres"
	postgresPod         = "postgres-0"

	readWriteSecret = "substrate-postgres-readwrite"
	readWriteKey    = "readWriteConnectionString"
	ownerSecret     = "substrate-postgres-owner"
	ownerKey        = "ownerConnectionString"
)

// applyPostgres applies manifests/ate-install/kind/postgres and waits for it.
// That overlay sizes the database for evaluation rather than production.
func (in *Installer) applyPostgres(ctx context.Context, progress func(string)) (string, error) {
	c := in.opts.Config
	sc, err := postgresStorageClass(ctx, in.kube, c.StorageClass)
	if err != nil {
		return "", err
	}
	c.StorageClass = sc
	manifest, err := postgresManifest(c)
	if err != nil {
		return "", err
	}
	if err := applyManifest(ctx, in.kube, manifest); err != nil {
		return "", err
	}
	progress("waiting for statefulset/" + postgresStatefulSet)
	if err := waitStatefulSetReady(ctx, in.kube, SystemNamespace, postgresStatefulSet, in.opts.Timeout.Duration); err != nil {
		return "", err
	}
	return "statefulset/" + postgresStatefulSet + " ready", nil
}

// postgresStorageClass returns the StorageClass to apply the bundled
// PostgreSQL with. An existing StatefulSet keeps its own, since its
// volumeClaimTemplates can't change.
func postgresStorageClass(ctx context.Context, kc kubernetes.Interface, sc string) (string, error) {
	ss, err := kc.AppsV1().StatefulSets(SystemNamespace).Get(ctx, postgresStatefulSet, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return sc, nil
	}
	if err != nil {
		return "", err
	}
	return ptr.Deref(ss.Spec.VolumeClaimTemplates[0].Spec.StorageClassName, ""), nil
}

// postgresManifest builds the bundled PostgreSQL manifest. c's image registry
// replaces the image's, the way the charts' global.imageRegistry does, and c's
// StorageClass sets the volume's.
func postgresManifest(c Config) ([]byte, error) {
	mem := filesys.MakeFsInMemory()
	err := fs.WalkDir(ateinstall.Postgres, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(ateinstall.Postgres, p)
		if err != nil {
			return err
		}
		return mem.WriteFile("/"+p, data)
	})
	if err != nil {
		return nil, err
	}
	dir := "/kind/postgres"
	overlay := kusttypes.Kustomization{
		TypeMeta:  kusttypes.TypeMeta{APIVersion: kusttypes.KustomizationVersion, Kind: kusttypes.KustomizationKind},
		Resources: []string{"../kind/postgres"},
	}
	if r := c.ImageRegistry; r != "" {
		overlay.Images = []kusttypes.Image{{Name: "postgres", NewName: r + "/postgres"}}
	}
	if sc := c.StorageClass; sc != "" {
		patch, err := yaml.Marshal([]map[string]any{{
			"op":    "add",
			"path":  "/spec/volumeClaimTemplates/0/spec/storageClassName",
			"value": sc,
		}})
		if err != nil {
			return nil, err
		}
		overlay.Patches = []kusttypes.Patch{{
			Target: &kusttypes.Selector{ResId: resid.NewResIdWithNamespace(resid.NewGvk("apps", "v1", "StatefulSet"), postgresStatefulSet, SystemNamespace)},
			Patch:  string(patch),
		}}
	}
	if overlay.Images != nil || overlay.Patches != nil {
		k, err := yaml.Marshal(overlay)
		if err != nil {
			return nil, err
		}
		dir = "/overlay"
		if err := mem.WriteFile(dir+"/kustomization.yaml", k); err != nil {
			return nil, err
		}
	}
	opts := krusty.MakeDefaultOptions()
	// The overlay references its base outside its own directory.
	opts.LoadRestrictions = kusttypes.LoadRestrictionsNone
	res, err := krusty.MakeKustomizer(opts).Run(mem, dir)
	if err != nil {
		return nil, fmt.Errorf("while building the PostgreSQL manifest: %w", err)
	}
	return res.AsYaml()
}

// applyManifest server-side applies each object in a multi-document YAML. It
// handles the kinds the PostgreSQL manifest uses.
func applyManifest(ctx context.Context, kc kubernetes.Interface, manifest []byte) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	force := true
	opts := metav1.PatchOptions{FieldManager: fieldManager, Force: &force}
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj.Object)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("while decoding manifest: %w", err)
		}
		if obj.Object == nil {
			continue
		}
		data, err := obj.MarshalJSON()
		if err != nil {
			return err
		}
		ns, name := obj.GetNamespace(), obj.GetName()
		switch obj.GetKind() {
		case "ConfigMap":
			_, err = kc.CoreV1().ConfigMaps(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
		case "Service":
			_, err = kc.CoreV1().Services(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
		case "StatefulSet":
			_, err = kc.AppsV1().StatefulSets(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
		default:
			return fmt.Errorf("unsupported kind %s in manifest", obj.GetKind())
		}
		if err != nil {
			return fmt.Errorf("while applying %s %s/%s: %w", obj.GetKind(), ns, name, err)
		}
	}
}

func waitStatefulSetReady(ctx context.Context, kc kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		ss, err := kc.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		want := int32(1)
		if ss.Spec.Replicas != nil {
			want = *ss.Spec.Replicas
		}
		return ss.Status.ObservedGeneration >= ss.Generation &&
			ss.Status.UpdatedReplicas == want &&
			ss.Status.ReadyReplicas == want, nil
	})
	if err != nil {
		return fmt.Errorf("statefulset %s/%s not ready: %w", namespace, name, err)
	}
	return nil
}

// setupDatabase creates the Substrate schema and roles, then the connection
// Secrets the substrate chart reads. Both are idempotent.
func (in *Installer) setupDatabase(ctx context.Context, _ func(string)) (string, error) {
	cmd := append([]string{
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--single-transaction",
		"--username=postgres", "--dbname=atepg",
	}, postgressetup.DefaultConfig().PSQLArgs()...)
	var stderr bytes.Buffer
	if err := in.exec(ctx, SystemNamespace, postgresPod, "postgres", cmd, strings.NewReader(postgressetup.SQL()), io.Discard, &stderr); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var created []string
	for _, s := range bundledConnectionSecrets() {
		ok, err := createIfAbsent(ctx, in.kube, s)
		if err != nil {
			return "", err
		}
		if ok {
			created = append(created, s.Name)
		}
	}
	return "schema ready; " + createdSummary(created, 2), nil
}

// bundledConnectionSecrets connect with pod certificates, which
// postgres.clientCertificates.enabled projects into ate-api-server.
func bundledConnectionSecrets() []*corev1.Secret {
	const tls = "sslmode=verify-full&sslrootcert=/run/servicedns.podcert.ate.dev/trust-bundle.pem"
	conn := func(user string) string {
		cert := "/run/postgres.podcert.ate.dev/" + user + ".pem"
		return "postgresql://" + user + "@postgres." + SystemNamespace + ".svc:5432/atepg?" + tls + "&sslcert=" + cert + "&sslkey=" + cert
	}
	return []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: readWriteSecret},
			StringData: map[string]string{readWriteKey: conn(postgressetup.ReadWriteUser)},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: ownerSecret},
			StringData: map[string]string{ownerKey: conn(postgressetup.OwnerUser)},
		},
	}
}

// checkDatabaseSecrets verifies the connection Secrets for an external
// database. Their names and keys are the substrate chart's defaults.
func (in *Installer) checkDatabaseSecrets(ctx context.Context, _ func(string)) (string, error) {
	for _, ref := range []struct{ name, key string }{{readWriteSecret, readWriteKey}, {ownerSecret, ownerKey}} {
		s, err := in.kube.CoreV1().Secrets(SystemNamespace).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("while reading secret %s/%s: %w; see charts/substrate/README.md for its format", SystemNamespace, ref.name, err)
		}
		if len(s.Data[ref.key]) == 0 {
			return "", fmt.Errorf("secret %s/%s has no %s key", SystemNamespace, ref.name, ref.key)
		}
	}
	return "found " + readWriteSecret + " and " + ownerSecret, nil
}

func (in *Installer) exec(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := in.kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(in.rest, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("while creating exec stream for pod %s/%s: %w", namespace, pod, err)
	}
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr}); err != nil {
		return fmt.Errorf("exec in pod %s/%s: %w", namespace, pod, err)
	}
	return nil
}
