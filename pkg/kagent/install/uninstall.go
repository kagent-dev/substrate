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
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"go.ate.dev/substrate/internal/clustertrustbundle"
)

// ErrAborted is returned when UninstallOptions.Confirm declines an uninstall.
var ErrAborted = errors.New("aborted")

var workerPoolsGVR = atev1.GroupVersion.WithResource("workerpools")

// UninstallOptions configures Uninstall.
type UninstallOptions struct {
	// IncludeCRDs deletes the CRDs and every custom resource.
	IncludeCRDs bool
	// Purge deletes the namespaces, with their CA keys, secrets and database
	// volume, and the published trust bundles.
	Purge bool
	// Confirm is shown what Uninstall will delete and returns whether to go
	// on. Nil means go on.
	Confirm func(ctx context.Context, summary string) (bool, error)
}

// Uninstall removes the releases in the reverse of install order. It keeps
// CRDs, secrets, the database volume and the namespaces unless
// opts.IncludeCRDs or opts.Purge say otherwise.
func (in *Installer) Uninstall(ctx context.Context, opts UninstallOptions) error {
	summary, err := in.inventory(ctx, opts)
	if err != nil {
		return err
	}
	if opts.Confirm != nil {
		ok, err := opts.Confirm(ctx, summary)
		if err != nil {
			return err
		}
		if !ok {
			return ErrAborted
		}
	}
	steps := []Step{
		{"Delete worker pools", in.deleteWorkerPools},
		{"Uninstall Substrate", in.uninstallStep(substrateRelease)},
		{"Remove PostgreSQL", in.removePostgres},
		{"Uninstall atelet", in.uninstallStep(ateletRelease)},
		{"Uninstall certificate controller", in.uninstallStep(podCertRelease)},
	}
	if opts.IncludeCRDs {
		steps = append(steps, Step{"Uninstall CRDs", in.uninstallStep(crdsRelease)})
	}
	if opts.Purge {
		steps = append(steps,
			Step{"Delete trust bundles", in.deleteTrustBundles},
			Step{"Delete namespaces", in.deleteNamespaces},
		)
	}
	return in.Run(ctx, steps)
}

// inventory describes what Uninstall will remove, for opts.Confirm.
func (in *Installer) inventory(ctx context.Context, opts UninstallOptions) (string, error) {
	var installed []string
	for _, r := range allReleases {
		rel, err := in.helm.get(r)
		if err != nil {
			return "", err
		}
		if rel != nil && (r != crdsRelease || opts.IncludeCRDs) {
			installed = append(installed, r.name)
		}
	}
	pools, err := in.dyn.Resource(workerPoolsGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Releases to uninstall: %s\n", orNone(installed))
	if pools != nil {
		fmt.Fprintf(&b, "WorkerPools to delete: %d, and every actor running on them\n", len(pools.Items))
	}
	if opts.IncludeCRDs {
		b.WriteString("CRDs to delete, with every WorkerPool, SandboxConfig and CSIDriverConfig\n")
	}
	if opts.Purge {
		fmt.Fprintf(&b, "Namespaces to delete: %s and %s, with their CA keys, secrets and database volume\n", SystemNamespace, PodCertNamespace)
	}
	return b.String(), nil
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// deleteWorkerPools runs while ate-controller still does, so it can remove
// the SandboxConfig protection finalizers. Without that, uninstalling the
// substrate release would hang on gvisor-default.
func (in *Installer) deleteWorkerPools(ctx context.Context, progress func(string)) (string, error) {
	pools := in.dyn.Resource(workerPoolsGVR)
	list, err := pools.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return "CRDs not installed", nil
	}
	if err != nil {
		return "", err
	}
	for _, p := range list.Items {
		err := pools.Namespace(p.GetNamespace()).Delete(ctx, p.GetName(), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	err = in.poll(ctx, func(ctx context.Context) (bool, error) {
		left, err := pools.List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		progress(fmt.Sprintf("%d WorkerPools left", len(left.Items)))
		return len(left.Items) == 0, nil
	})
	if err != nil {
		return "", fmt.Errorf("while waiting for WorkerPools to go; ate-controller must be running to finalize them: %w", err)
	}
	released, err := in.releaseSandboxConfigs(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d deleted, %d SandboxConfigs released", len(list.Items), released), nil
}

// releaseSandboxConfigs removes the WorkerPool protection finalizer from every
// SandboxConfig. ate-controller only removes it while a SandboxConfig is being
// deleted, and helm uninstall deletes ate-controller and the chart's
// SandboxConfig together. No WorkerPool is left, so the finalizer protects
// nothing.
func (in *Installer) releaseSandboxConfigs(ctx context.Context) (int, error) {
	res := in.dyn.Resource(sandboxConfigsGVR)
	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("while listing SandboxConfigs: %w", err)
	}
	n := 0
	for _, item := range list.Items {
		name := item.GetName()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			sc, err := res.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			finalizers := slices.DeleteFunc(sc.GetFinalizers(), func(f string) bool {
				return f == atev1.SandboxConfigWorkerPoolProtectionFinalizer
			})
			if len(finalizers) == len(sc.GetFinalizers()) {
				return nil
			}
			sc.SetFinalizers(finalizers)
			if _, err := res.Update(ctx, sc, metav1.UpdateOptions{}); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("while releasing SandboxConfig %s: %w", name, err)
		}
	}
	return n, nil
}

func (in *Installer) uninstallStep(r release) func(context.Context, func(string)) (string, error) {
	return func(ctx context.Context, _ func(string)) (string, error) {
		ok, err := in.helm.uninstall(ctx, r, in.opts.Timeout.Duration)
		if err != nil {
			return "", err
		}
		if !ok {
			return "not installed", nil
		}
		return "uninstalled", nil
	}
}

// removePostgres deletes the bundled PostgreSQL objects that install applied,
// identified by kubectl ate's field manager. Its volume stays until --purge.
func (in *Installer) removePostgres(ctx context.Context, _ func(string)) (string, error) {
	manifest, err := postgresManifest()
	if err != nil {
		return "", err
	}
	var deleted []string
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		gvr, ok := postgresKinds[obj.GetKind()]
		if !ok {
			return "", fmt.Errorf("unsupported kind %s in manifest", obj.GetKind())
		}
		res := in.dyn.Resource(gvr).Namespace(obj.GetNamespace())
		live, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !managedByUs(live) {
			continue
		}
		if err := res.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		deleted = append(deleted, strings.ToLower(obj.GetKind())+"/"+obj.GetName())
	}
	if len(deleted) == 0 {
		return "not installed", nil
	}
	return "deleted " + strings.Join(deleted, ", "), nil
}

// postgresKinds maps the kinds in the PostgreSQL manifest to resources.
var postgresKinds = map[string]schema.GroupVersionResource{
	"ConfigMap":   {Version: "v1", Resource: "configmaps"},
	"Service":     {Version: "v1", Resource: "services"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
}

func managedByUs(obj *unstructured.Unstructured) bool {
	for _, m := range obj.GetManagedFields() {
		if m.Manager == fieldManager {
			return true
		}
	}
	return false
}

// substrateTrustBundles maps the bundles the certificate controller and
// ate-controller publish to their signers. They are cluster-scoped, so
// deleting the namespaces leaves them. A bundle is deleted only when its name
// and signer both match, because anyone who may attest for a signer can
// publish under another name.
var substrateTrustBundles = map[string]string{
	"podidentity.podcert.ate.dev:identity:primary-bundle": "podidentity.podcert.ate.dev/identity",
	"servicedns.podcert.ate.dev:identity:primary-bundle":  "servicedns.podcert.ate.dev/identity",
	"postgres.podcert.ate.dev:identity:primary-bundle":    "postgres.podcert.ate.dev/identity",
	"egress-mitm.ate.dev:mitm:primary-bundle":             "egress-mitm.ate.dev/mitm",
}

func (in *Installer) deleteTrustBundles(ctx context.Context, _ func(string)) (string, error) {
	ctbs, err := clustertrustbundle.NewClient(in.kube, nil)
	if err != nil {
		return "", err
	}
	list, err := ctbs.List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	n := 0
	for _, b := range list.Items {
		if signer, ok := substrateTrustBundles[b.Name]; !ok || b.Spec.SignerName != signer {
			continue
		}
		if err := ctbs.Delete(ctx, b.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		n++
	}
	return fmt.Sprintf("%d deleted", n), nil
}

func (in *Installer) deleteNamespaces(ctx context.Context, progress func(string)) (string, error) {
	namespaces := []string{SystemNamespace, PodCertNamespace}
	for _, ns := range namespaces {
		err := in.kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	err := in.poll(ctx, func(ctx context.Context) (bool, error) {
		var left []string
		for _, ns := range namespaces {
			_, err := in.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			left = append(left, ns)
		}
		if len(left) > 0 {
			progress("waiting for " + strings.Join(left, ", "))
		}
		return len(left) == 0, nil
	})
	if err != nil {
		return "", err
	}
	return strings.Join(namespaces, ", ") + " deleted", nil
}

// poll calls cond every 2 seconds until it returns true or Options.Timeout
// passes.
func (in *Installer) poll(ctx context.Context, cond wait.ConditionWithContextFunc) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, in.opts.Timeout.Duration, true, cond)
}
