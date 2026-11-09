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

	"github.com/Masterminds/semver/v3"
	"github.com/pmezard/go-difflib/difflib"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// upgradeOrder follows steps 1 and 2 of docs/reference/version-skew-policy.md.
// The substrate release holds steps 4 to 8, so the CRDs go before it. Worker
// images (step 3) are left to the user.
var upgradeOrder = []struct {
	r    release
	name string
}{
	{podCertRelease, "Upgrade certificate controller"},
	{ateletRelease, "Upgrade atelet"},
	{crdsRelease, "Upgrade CRDs"},
	{substrateRelease, "Upgrade Substrate"},
}

// UpgradeOptions configures Upgrade.
type UpgradeOptions struct {
	// DryRun writes each release's manifest diff to Out instead of applying.
	DryRun bool
	Out    io.Writer
}

// Upgrade runs UpgradeSteps.
func (in *Installer) Upgrade(ctx context.Context, opts UpgradeOptions) error {
	steps, err := in.UpgradeSteps(ctx, opts)
	if err != nil {
		return err
	}
	return in.Run(ctx, steps)
}

// UpgradeSteps returns the steps that move every release to the charts in
// this binary. It reads the cluster to check for the microVM SandboxConfig.
// After it is called, the Installer's helm steps upgrade releases across
// chart versions.
func (in *Installer) UpgradeSteps(ctx context.Context, opts UpgradeOptions) ([]Step, error) {
	in.upgrading = true
	steps := []Step{
		{"Check versions", in.checkVersions},
		{"Check CRD storage versions", in.checkCRDStorage},
	}
	if opts.DryRun {
		for _, u := range upgradeOrder {
			steps = append(steps, Step{"Diff " + u.r.name, in.diffStep(u.r, in.valuesFor(u.r), opts.Out)})
		}
		return steps, nil
	}
	steps = append(steps,
		Step{"Create pod certificate CAs", in.createPodCertCAs},
		Step{upgradeOrder[0].name, in.helmStep(podCertRelease, noValues)},
		Step{"Wait for trust bundles", in.waitTrustBundles},
		Step{"Create identity secrets", in.createIdentitySecrets},
	)
	for _, u := range upgradeOrder[1:] {
		steps = append(steps, Step{u.name, in.helmStep(u.r, in.valuesFor(u.r))})
	}
	// A new release may pin new microVM assets.
	microVM, err := in.hasMicroVMSandboxConfig(ctx)
	if err != nil {
		return nil, err
	}
	if microVM {
		steps = append(steps,
			Step{"Stage microVM assets", in.stageMicroVMAssets},
			Step{"Apply microVM SandboxConfig", in.applyMicroVMSandboxConfig},
		)
	}
	steps = append(steps, Step{"Check API", in.checkAPI})
	return steps, nil
}

// hasMicroVMSandboxConfig reports whether the microvm SandboxConfig exists.
func (in *Installer) hasMicroVMSandboxConfig(ctx context.Context) (bool, error) {
	_, err := in.dyn.Resource(sandboxConfigsGVR).Get(ctx, microVMSandboxConfig, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (in *Installer) valuesFor(r release) valuesFunc {
	switch r {
	case ateletRelease:
		return in.ateletValues
	case substrateRelease:
		return in.substrateValues
	}
	return noValues
}

// checkVersions checks every release is installed, settled, and allowed to
// move to this binary's chart version.
func (in *Installer) checkVersions(_ context.Context, progress func(string)) (string, error) {
	var from, to string
	for _, r := range allReleases {
		cur, err := in.helm.get(r)
		if err != nil {
			return "", err
		}
		if cur == nil {
			if r == ateletRelease {
				return "", errors.New("release substrate-atelet isn't installed; if the substrate release runs atelet (atelet.enabled=true), reinstall as charts/substrate-atelet/README.md describes")
			}
			return "", fmt.Errorf("release %s isn't installed; run kubectl ate install", r.name)
		}
		if st := cur.Info.Status; st.IsPending() {
			return "", fmt.Errorf("release %s is %s; run helm rollback or helm uninstall on it, then retry", r.name, st)
		}
		ch, err := loadChart(in.charts, r.chart)
		if err != nil {
			return "", err
		}
		from, to = cur.Chart.Metadata.Version, ch.Metadata.Version
		if err := checkSkew(from, to); err != nil {
			return "", fmt.Errorf("release %s: %w", r.name, err)
		}
		progress(fmt.Sprintf("%s %s → %s", r.name, from, to))
	}
	if from == to {
		return to + ", reapplying", nil
	}
	return from + " → " + to, nil
}

// checkSkew applies docs/reference/version-skew-policy.md to a chart version
// change. Before 1.0 every minor release needs a reinstall.
func checkSkew(from, to string) error {
	f, err := semver.StrictNewVersion(from)
	if err != nil {
		return fmt.Errorf("installed version %q: %w", from, err)
	}
	t, err := semver.StrictNewVersion(to)
	if err != nil {
		return fmt.Errorf("target version %q: %w", to, err)
	}
	switch {
	case t.LessThan(f):
		return fmt.Errorf("downgrading %s to %s isn't supported; roll back with helm rollback in the reverse of the upgrade order", from, to)
	case f.Major() == t.Major() && f.Minor() == t.Minor():
		return nil
	case f.Major() == 0:
		return fmt.Errorf("upgrading %s to %s needs a reinstall: releases before 1.0 aren't compatible across minor versions", from, to)
	case f.Major() == t.Major() && t.Minor() == f.Minor()+1:
		return nil
	case f.Major() == t.Major():
		return fmt.Errorf("can't skip minor releases: upgrade %s to %d.%d first", from, f.Major(), f.Minor()+1)
	}
	return fmt.Errorf("upgrading %s to %s crosses a major version; follow that release's upgrade notes", from, to)
}

// checkCRDStorage refuses to drop a CRD version that objects are still
// stored in, which would make them unreadable.
func (in *Installer) checkCRDStorage(ctx context.Context, _ func(string)) (string, error) {
	ch, err := loadChart(in.charts, crdsRelease.chart)
	if err != nil {
		return "", err
	}
	var manifest bytes.Buffer
	for _, t := range ch.Templates {
		manifest.Write(t.Data)
		manifest.WriteString("\n---\n")
	}
	crds, err := decodeCRDs(manifest.Bytes())
	if err != nil {
		return "", err
	}
	if err := checkStoredVersions(ctx, in.apiext, crds); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d CRDs compatible", len(crds)), nil
}

func decodeCRDs(manifest []byte) ([]apiextv1.CustomResourceDefinition, error) {
	var out []apiextv1.CustomResourceDefinition
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var crd apiextv1.CustomResourceDefinition
		err := dec.Decode(&crd)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("while decoding CRDs: %w", err)
		}
		if crd.Kind == "CustomResourceDefinition" {
			out = append(out, crd)
		}
	}
}

func checkStoredVersions(ctx context.Context, c apiextclient.Interface, crds []apiextv1.CustomResourceDefinition) error {
	for _, crd := range crds {
		live, err := c.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		var versions []string
		for _, v := range crd.Spec.Versions {
			versions = append(versions, v.Name)
		}
		for _, stored := range live.Status.StoredVersions {
			if !slices.Contains(versions, stored) {
				return fmt.Errorf("CRD %s still stores objects as %s, which the new chart drops; migrate them to %s first", crd.Name, stored, strings.Join(versions, ", "))
			}
		}
	}
	return nil
}

// diffStep writes the change an upgrade of r would make to its manifest to
// out.
func (in *Installer) diffStep(r release, vf valuesFunc, out io.Writer) func(context.Context, func(string)) (string, error) {
	return func(ctx context.Context, _ func(string)) (string, error) {
		ch, err := loadChart(in.charts, r.chart)
		if err != nil {
			return "", err
		}
		cur, err := in.helm.get(r)
		if err != nil {
			return "", err
		}
		vals, err := in.releaseValues(ctx, r, vf, false)
		if err != nil {
			return "", err
		}
		next, err := in.helm.render(ctx, r, ch, vals)
		if err != nil {
			return "", err
		}
		n, err := writeDiff(out, r.name, cur.Manifest, next)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "no changes", nil
		}
		return fmt.Sprintf("%d lines changed", n), nil
	}
}

// writeDiff writes a unified diff of two manifests and returns how many lines
// it adds or removes.
func writeDiff(w io.Writer, name, from, to string) (int, error) {
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(from),
		B:        difflib.SplitLines(to),
		FromFile: name + " (installed)",
		ToFile:   name + " (upgrade)",
		Context:  3,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if (strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++")) || (strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---")) {
			n++
		}
	}
	if n > 0 {
		_, err = io.WriteString(w, text)
	}
	return n, err
}
