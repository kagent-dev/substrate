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
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Status writes the releases, workload readiness and bootstrap secrets.
func (in *Installer) Status(ctx context.Context, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)

	fmt.Fprintln(tw, "RELEASE\tNAMESPACE\tCHART\tAPP\tREVISION\tSTATUS\tUPDATED")
	apps := map[release]string{}
	for _, r := range allReleases {
		rel, err := in.helm.get(r)
		if err != nil {
			return err
		}
		if rel == nil {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\tnot installed\t-\n", r.name, r.namespace)
			continue
		}
		apps[r] = rel.Chart.Metadata.AppVersion
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.name, r.namespace,
			rel.Chart.Metadata.Version, rel.Chart.Metadata.AppVersion, rel.Version,
			rel.Info.Status, rel.Info.LastDeployed.Format(time.RFC3339))
	}
	if a, s := apps[ateletRelease], apps[substrateRelease]; a != "" && s != "" && a != s {
		fmt.Fprintf(tw, "\natelet runs %s and the control plane %s; finish the upgrade with kubectl ate upgrade\n", a, s)
	}

	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "WORKLOAD\tNAMESPACE\tREADY")
	for _, ns := range []string{PodCertNamespace, SystemNamespace} {
		rows, err := in.workloads(ctx, ns)
		if err != nil {
			return err
		}
		for _, row := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\n", row.name, ns, row.ready, row.desired)
		}
	}

	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "SECRET\tNAMESPACE\tPRESENT")
	type ref struct{ namespace, name string }
	var secrets []ref
	for _, p := range podCertCAPools {
		secrets = append(secrets, ref{p.namespace, p.name})
	}
	for _, name := range []string{"actor-id-jwt-pool", actorIDCAPool.name, "actor-id-ca-certs", egressMITMCAPool.name, rustfsCredentialsSecret, readWriteSecret, ownerSecret} {
		secrets = append(secrets, ref{SystemNamespace, name})
	}
	for _, s := range secrets {
		ok, err := secretExists(ctx, in.kube, s.namespace, s.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\n", s.name, s.namespace, ok)
	}
	return tw.Flush()
}

type workload struct {
	name           string
	ready, desired int32
}

func (in *Installer) workloads(ctx context.Context, ns string) ([]workload, error) {
	var out []workload
	deps, err := in.kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range deps.Items {
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		out = append(out, workload{"deployment/" + d.Name, d.Status.ReadyReplicas, want})
	}
	dss, err := in.kube.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range dss.Items {
		out = append(out, workload{"daemonset/" + d.Name, d.Status.NumberReady, d.Status.DesiredNumberScheduled})
	}
	sss, err := in.kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, s := range sss.Items {
		want := int32(1)
		if s.Spec.Replicas != nil {
			want = *s.Spec.Replicas
		}
		out = append(out, workload{"statefulset/" + s.Name, s.Status.ReadyReplicas, want})
	}
	return out, nil
}
