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

package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.ate.dev/substrate/pkg/kagent/install"
)

func TestView(t *testing.T) {
	r := New(&bytes.Buffer{}, &bytes.Buffer{}, "install", func() {})
	m := r.model
	for _, e := range []install.Event{
		{Kind: install.Note, Msg: "evaluation only"},
		{Kind: install.Plan, Step: "Check cluster"},
		{Kind: install.Plan, Step: "Install CRDs"},
		{Kind: install.Plan, Step: "Install atelet"},
		{Kind: install.StepStarted, Step: "Check cluster"},
		{Kind: install.StepDone, Step: "Check cluster", Msg: "Kubernetes v1.37.0"},
		{Kind: install.StepStarted, Step: "Install CRDs"},
		{Kind: install.StepProgress, Step: "Install CRDs", Msg: "p1"},
		{Kind: install.StepProgress, Step: "Install CRDs", Msg: "p2"},
		{Kind: install.StepProgress, Step: "Install CRDs", Msg: "p3"},
		{Kind: install.StepProgress, Step: "Install CRDs", Msg: "p4"},
	} {
		m.apply(e)
	}
	v := m.View()
	for _, want := range []string{"evaluation only", "✓ Check cluster: Kubernetes v1.37.0", "Install CRDs", "p4", "○ Install atelet"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "p1") {
		t.Errorf("View() shows more than %d progress lines:\n%s", progressLines, v)
	}

	m.apply(install.Event{Kind: install.StepFailed, Step: "Install CRDs"})
	m.Update(finishMsg{err: errors.New("Install CRDs: boom")})
	v = m.View()
	for _, want := range []string{"✗ Install CRDs", "Install CRDs: boom"} {
		if !strings.Contains(v, want) {
			t.Errorf("final View() missing %q:\n%s", want, v)
		}
	}
}

func TestHint(t *testing.T) {
	for _, cfg := range []install.Config{
		install.DefaultConfig(),
		func() install.Config {
			c := install.DefaultConfig()
			c.Postgres = install.Postgres{Type: install.PostgresExternal}
			c.ObjectStore = install.ObjectStore{
				Type:   install.ObjectStoreGCS,
				Bucket: "snapshots",
				GCS:    install.GCSObjectStore{TransferServiceAccount: "sa@p.iam.gserviceaccount.com"},
			}
			c.MicroVM = install.MicroVM{Enabled: true, AssetBucket: "assets"}
			c.Releases.All.ValuesFiles = []string{"/etc/gke.yaml"}
			return c
		}(),
		func() install.Config {
			c := install.DefaultConfig()
			c.Profile, c.AKS.ACR = install.ProfileAKS, "r.azurecr.io"
			c.AKS.ManagedPrometheus = true
			c.OTelEndpoint = "http://collector:4317"
			return c
		}(),
	} {
		hint, err := Hint(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, doc, ok := strings.Cut(hint, ":\n\n")
		if !ok {
			t.Fatalf("Hint() has no config:\n%s", hint)
		}
		got, err := install.ReadConfig(strings.NewReader(doc), "", "")
		if err != nil {
			t.Fatalf("ReadConfig(Hint()): %v\n%s", err, doc)
		}
		if diff := cmp.Diff(cfg, got); diff != "" {
			t.Errorf("ReadConfig(Hint()) mismatch (-want +got):\n%s", diff)
		}
	}
}
