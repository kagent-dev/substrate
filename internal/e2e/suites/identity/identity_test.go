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

package identity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	probeNamespace = "ate-e2e-probe"
	probeTemplate  = "probe"
)

type whoamiResponse struct {
	File     string `json:"file"`
	Hostname string `json:"hostname"`
	// Error is the probe's identity-file read error, if any, so a failed
	// assertion explains why the ID was missing.
	Error string `json:"error"`
}

// TestActorIdentity_AfterRestore_IsOwnID_NotGolden is the regression gate for
// per-actor identity. The env-var approach passed unit tests and config.json
// inspection yet was broken at runtime: actors restored from the shared golden
// snapshot all reported the golden actor's ID. This test catches that by
// restoring TWO actors from one golden snapshot and asserting each observes its
// OWN id — and explicitly that it is not the golden id.
func TestActorIdentity_AfterRestore_IsOwnID_NotGolden(t *testing.T) {
	env, err := e2e.CheckEnv("BUCKET_NAME", "KO_DOCKER_REPO")
	if err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	ctx := context.Background()
	clients := e2e.GetClients()

	deployProbe(t, ctx, clients, env["BUCKET_NAME"])
	golden, at := waitForGolden(t, ctx, clients)
	e2e.GrantAtespaceToTemplateWorkerPools(t, ctx, clients, probeNamespace, at)

	// Two distinct actors from the same golden snapshot.
	ids := []string{"probe-alpha", "probe-beta"}
	for _, id := range ids {
		createAndResumeActor(t, ctx, clients, id)
	}

	rc, err := e2e.NewRouterClient(ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	defer rc.Close()

	seen := map[string]string{}
	for _, id := range ids {
		got := whoami(t, ctx, rc, id)

		if got.File != id {
			t.Errorf("actor %q: /run/ate/actor-id = %q, want %q (probe read error: %q)", id, got.File, id, got.Error)
		}
		if got.File == golden {
			t.Errorf("actor %q: identity is the GOLDEN snapshot id %q — restore leaked shared state", id, golden)
		}
		if other, dup := seen[got.File]; dup {
			t.Errorf("actor %q and %q both report identity %q — actors are not distinct", id, other, got.File)
		}
		seen[got.File] = id
	}
}

func deployProbe(t *testing.T, ctx context.Context, clients *e2e.Clients, bucket string) {
	t.Helper()

	_, err := clients.K8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: probeNamespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", probeNamespace, err)
	}
	t.Cleanup(func() {
		_ = clients.K8s.CoreV1().Namespaces().Delete(context.Background(), probeNamespace, metav1.DeleteOptions{})
	})

	e2e.EnsureGvisorSandboxConfig(t, ctx, clients)
	ateomImage := e2e.KoBuild(t, "./cmd/ateom-gvisor")
	probeImage := e2e.KoBuild(t, "./internal/e2e/fixtures/probe")

	_, _ = clients.SubstrateAPI.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{Name: probeNamespace})
	_, err = clients.SubstrateAPI.CreateWorkerPool(ctx, &ateapipb.CreateWorkerPoolRequest{
		WorkerPool: &ateapipb.WorkerPool{
			Namespace: probeNamespace,
			Name:      probeTemplate,
			Labels:    map[string]string{"workload": probeTemplate},
			Spec: &ateapipb.WorkerPoolSpec{
				Replicas:     3,
				AteomImage:   ateomImage,
				SandboxClass: string(v1alpha1.SandboxClassGvisor),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkerPool %s/%s: %v", probeNamespace, probeTemplate, err)
	}

	_, err = clients.SubstrateAPI.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{
		ActorTemplate: &ateapipb.ActorTemplate{
			Atespace: probeNamespace,
			Name:     probeTemplate,
			Spec: &ateapipb.ActorTemplateSpec{
				PauseImage: e2e.PauseImage,
				Containers: []*ateapipb.Container{{
					Name:    probeTemplate,
					Image:   probeImage,
					Command: []string{"/ko-app/probe"},
				}},
				WorkerSelector: &ateapipb.LabelSelector{MatchLabels: map[string]string{"workload": probeTemplate}},
				SnapshotsConfig: &ateapipb.SnapshotsConfig{
					Location: "gs://" + bucket + "/ate-e2e-probe/",
				},
				SandboxClass: string(v1alpha1.SandboxClassGvisor),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateActorTemplate %s/%s: %v", probeNamespace, probeTemplate, err)
	}
}

func waitForGolden(t *testing.T, ctx context.Context, clients *e2e.Clients) (string, *v1alpha1.ActorTemplate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := clients.SubstrateAPI.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
			ActorTemplate: &ateapipb.ActorTemplateRef{Atespace: probeNamespace, Name: probeTemplate},
		})
		if err == nil {
			at := actorTemplateAPI(resp.GetActorTemplate())
			switch at.Status.Phase {
			case v1alpha1.PhaseReady:
				t.Logf("probe ActorTemplate ready, golden=%s", at.Status.GoldenActorID)
				return at.Status.GoldenActorID, at
			case v1alpha1.PhaseFailed:
				t.Fatalf("probe ActorTemplate entered PhaseFailed")
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for probe ActorTemplate to be Ready")
	return "", nil
}

func actorTemplateAPI(at *ateapipb.ActorTemplate) *v1alpha1.ActorTemplate {
	out := &v1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: at.GetAtespace(), Name: at.GetName()},
		Spec: v1alpha1.ActorTemplateSpec{
			SandboxClass: v1alpha1.SandboxClass(at.GetSpec().GetSandboxClass()),
		},
		Status: v1alpha1.ActorTemplateStatus{
			Phase:          v1alpha1.PhaseType(at.GetStatus().GetPhase()),
			GoldenActorID:  at.GetStatus().GetGoldenActorId(),
			GoldenSnapshot: at.GetStatus().GetGoldenSnapshot(),
		},
	}
	if at.GetSpec().GetWorkerSelector() != nil {
		out.Spec.WorkerSelector = &metav1.LabelSelector{
			MatchLabels: at.GetSpec().GetWorkerSelector().GetMatchLabels(),
		}
	}
	return out
}

func createAndResumeActor(t *testing.T, ctx context.Context, clients *e2e.Clients, id string) {
	t.Helper()
	// CreateActor requires the atespace to exist first.
	_, _ = clients.SubstrateAPI.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{Name: probeNamespace})
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorRef:               &ateapipb.ActorRef{Atespace: probeNamespace, Name: id},
		ActorTemplateNamespace: probeNamespace,
		ActorTemplateName:      probeTemplate,
	}); err != nil {
		t.Fatalf("CreateActor %q: %v", id, err)
	}
	t.Cleanup(func() {
		// DeleteActor requires the actor to be suspended.
		_, _ = clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorRef: &ateapipb.ActorRef{Atespace: probeNamespace, Name: id}})
		_, _ = clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorRef: &ateapipb.ActorRef{Atespace: probeNamespace, Name: id}})
	})

	// Resume from the golden snapshot (the restore path, not --boot).
	if _, err := clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorRef: &ateapipb.ActorRef{Atespace: probeNamespace, Name: id}}); err != nil {
		t.Fatalf("ResumeActor %q: %v", id, err)
	}
}

func whoami(t *testing.T, ctx context.Context, rc *e2e.RouterClient, id string) whoamiResponse {
	t.Helper()
	resp, err := rc.Get(ctx, probeNamespace, id, "/whoami")
	if err != nil {
		t.Fatalf("GET /whoami for %q: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /whoami for %q: status %d, body %q", id, resp.StatusCode, body)
	}
	var out whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding /whoami for %q: %v", id, err)
	}
	return out
}
