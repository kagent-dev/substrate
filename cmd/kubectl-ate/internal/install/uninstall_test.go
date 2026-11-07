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
	"slices"
	"testing"
	"time"

	certsv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
)

func newDynamic(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	lists := map[schema.GroupVersionResource]string{
		workerPoolsGVR:    "WorkerPoolList",
		sandboxConfigsGVR: "SandboxConfigList",
	}
	for _, gvr := range postgresKinds {
		lists[gvr] = "List"
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)
}

func object(gvk schema.GroupVersionKind, namespace, name, manager string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	if manager != "" {
		u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: manager}})
	}
	return u
}

func TestDeleteWorkerPools(t *testing.T) {
	ctx := context.Background()
	sc := object(sandboxConfigsGVR.GroupVersion().WithKind("SandboxConfig"), "", "gvisor-default", "")
	sc.SetFinalizers([]string{atev1.SandboxConfigWorkerPoolProtectionFinalizer, "other"})
	dc := newDynamic(
		object(workerPoolsGVR.GroupVersion().WithKind("WorkerPool"), "team-a", "pool", ""),
		sc,
	)
	in := &Installer{dyn: dc, opts: Options{Config: Config{Timeout: metav1.Duration{Duration: 10 * time.Second}}}}
	msg, err := in.deleteWorkerPools(ctx, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1 deleted, 1 SandboxConfigs released"; msg != want {
		t.Errorf("deleteWorkerPools() = %q, want %q", msg, want)
	}
	got, err := dc.Resource(sandboxConfigsGVR).Get(ctx, "gvisor-default", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f := got.GetFinalizers(); !slices.Equal(f, []string{"other"}) {
		t.Errorf("SandboxConfig finalizers = %v, want [other]", f)
	}
	left, err := dc.Resource(workerPoolsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Errorf("%d WorkerPools left", len(left.Items))
	}
}

func TestRemovePostgresKeepsUnmanaged(t *testing.T) {
	ctx := context.Background()
	ss := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	svc := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	dc := newDynamic(
		object(ss, SystemNamespace, postgresStatefulSet, fieldManager),
		// A Service someone else manages under the same name stays.
		object(svc, SystemNamespace, "postgres", "kubectl"),
	)
	in := &Installer{dyn: dc}
	msg, err := in.removePostgres(ctx, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "deleted statefulset/postgres" {
		t.Errorf("removePostgres() = %q", msg)
	}
	_, err = dc.Resource(postgresKinds["StatefulSet"]).Namespace(SystemNamespace).Get(ctx, postgresStatefulSet, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("statefulset still present: %v", err)
	}
	if _, err := dc.Resource(postgresKinds["Service"]).Namespace(SystemNamespace).Get(ctx, "postgres", metav1.GetOptions{}); err != nil {
		t.Errorf("unmanaged service deleted: %v", err)
	}
}

func TestDeleteTrustBundles(t *testing.T) {
	ctx := context.Background()
	bundle := func(name, signer string) *certsv1.ClusterTrustBundle {
		return &certsv1.ClusterTrustBundle{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       certsv1.ClusterTrustBundleSpec{SignerName: signer},
		}
	}
	kc := fake.NewClientset(
		bundle("postgres.podcert.ate.dev:identity:primary-bundle", "postgres.podcert.ate.dev/identity"),
		bundle("egress-mitm.ate.dev:mitm:primary-bundle", "egress-mitm.ate.dev/mitm"),
		// Same signer, another name: not ours.
		bundle("egress-mitm.ate.dev:mitm:other", "egress-mitm.ate.dev/mitm"),
		// Our name, another signer: not ours.
		bundle("servicedns.podcert.ate.dev:identity:primary-bundle", "example.com/other"),
	)
	kc.Resources = []*metav1.APIResourceList{{GroupVersion: "certificates.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "clustertrustbundles"}}}}
	in := &Installer{kube: kc}
	msg, err := in.deleteTrustBundles(ctx, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "2 deleted" {
		t.Errorf("deleteTrustBundles() = %q, want 2 deleted", msg)
	}
	left, err := kc.CertificatesV1().ClusterTrustBundles().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range left.Items {
		names = append(names, b.Name)
	}
	slices.Sort(names)
	if want := []string{"egress-mitm.ate.dev:mitm:other", "servicedns.podcert.ate.dev:identity:primary-bundle"}; !slices.Equal(names, want) {
		t.Errorf("left %v, want %v", names, want)
	}
}
