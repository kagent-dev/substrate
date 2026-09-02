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

package atepg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/emptypb"
)

func receiveEffectiveEgressPolicyChange(t *testing.T, watch *store.EffectiveEgressPolicyWatch) store.EffectiveEgressPolicyChange {
	t.Helper()
	select {
	case change, ok := <-watch.Events:
		if !ok {
			t.Fatal("effective egress policy watch closed unexpectedly")
		}
		return change
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for effective egress policy change")
		return store.EffectiveEgressPolicyChange{}
	}
}

func TestWatchEffectiveEgressPolicyChanges_ActorAndPolicyMutations(t *testing.T) {
	p := setupPostgresPersistence(t)
	ctx := context.Background()
	createTestAtespace(t, p, "team-a")

	watch, err := p.WatchEffectiveEgressPolicyChanges(ctx)
	if err != nil {
		t.Fatalf("WatchEffectiveEgressPolicyChanges failed: %v", err)
	}
	defer watch.Close()

	actor, err := p.CreateActor(ctx, &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-a"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	actorRef := resources.ActorRefFromActor(actor)
	if change := receiveEffectiveEgressPolicyChange(t, watch); change.Actor != actorRef {
		t.Fatalf("CreateActor change = %v, want %v", change.Actor, actorRef)
	}

	policy, err := p.CreateEgressPolicy(ctx, actorRef, &ateapipb.EgressPolicy{
		Rules: []*ateapipb.EgressRule{{All: &emptypb.Empty{}}},
	})
	if err != nil {
		t.Fatalf("CreateEgressPolicy failed: %v", err)
	}
	if change := receiveEffectiveEgressPolicyChange(t, watch); change.Actor != actorRef {
		t.Fatalf("CreateEgressPolicy change = %v, want %v", change.Actor, actorRef)
	}

	if _, err := p.UpdateActor(ctx, actorRef, store.PreconditionFrom(actor), func(actor *ateapipb.Actor) error {
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		return nil
	}); err != nil {
		t.Fatalf("UpdateActor failed: %v", err)
	}
	if change := receiveEffectiveEgressPolicyChange(t, watch); change.Actor != actorRef {
		t.Fatalf("UpdateActor change = %v, want %v", change.Actor, actorRef)
	}

	_, err = p.UpdateEgressPolicy(ctx, actorRef, store.PreconditionFrom(policy), func(policy *ateapipb.EgressPolicy) error {
		policy.Rules = nil
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateEgressPolicy failed: %v", err)
	}
	if change := receiveEffectiveEgressPolicyChange(t, watch); change.Actor != actorRef {
		t.Fatalf("UpdateEgressPolicy change = %v, want %v", change.Actor, actorRef)
	}

	if _, err := p.DeleteEgressPolicy(ctx, actorRef); err != nil {
		t.Fatalf("DeleteEgressPolicy failed: %v", err)
	}
	if change := receiveEffectiveEgressPolicyChange(t, watch); change.Actor != actorRef {
		t.Fatalf("DeleteEgressPolicy change = %v, want %v", change.Actor, actorRef)
	}
}

func TestEffectiveEgressPolicyInvalidation_RollsBackWithMutation(t *testing.T) {
	p := setupPostgresPersistence(t)
	ctx := context.Background()
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-a"}
	wantErr := errors.New("mutation failed")

	err := p.writeAndAppendEffectiveEgressPolicyChange(ctx, actorRef, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO atespaces (name, uid, version, proto) VALUES ('rolled-back', 'uid', 1, '')`); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("writeAndAppendEffectiveEgressPolicyChange error = %v, want %v", err, wantErr)
	}

	var atespaces, invalidations int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM atespaces WHERE name = 'rolled-back'`).Scan(&atespaces); err != nil {
		t.Fatalf("counting rolled-back atespaces: %v", err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM effective_egress_policy_outbox`).Scan(&invalidations); err != nil {
		t.Fatalf("counting effective-policy invalidations: %v", err)
	}
	if atespaces != 0 || invalidations != 0 {
		t.Fatalf("rolled-back transaction left %d atespaces and %d invalidations", atespaces, invalidations)
	}
}

func TestEffectiveEgressPolicyInvalidation_OnePerSuccessfulActorMutation(t *testing.T) {
	p := setupPostgresPersistence(t)
	ctx := context.Background()
	createTestAtespace(t, p, "team-a")
	actor, err := p.CreateActor(ctx, &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-a"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	actorRef := resources.ActorRefFromActor(actor)
	_, err = p.CreateEgressPolicy(ctx, actorRef, &ateapipb.EgressPolicy{})
	if err != nil {
		t.Fatalf("CreateEgressPolicy failed: %v", err)
	}

	wantErr := errors.New("mutation failed")
	if _, err := p.UpdateActor(ctx, actorRef, store.PreconditionFrom(actor), func(*ateapipb.Actor) error {
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("failed UpdateActor error = %v, want %v", err, wantErr)
	}

	deleting, err := p.UpdateActor(ctx, actorRef, store.PreconditionFrom(actor), func(actor *ateapipb.Actor) error {
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_DELETING
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateActor to deleting failed: %v", err)
	}
	var beforeDelete int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM effective_egress_policy_outbox`).Scan(&beforeDelete); err != nil {
		t.Fatalf("counting invalidations before Actor delete: %v", err)
	}
	if beforeDelete != 3 {
		t.Fatalf("invalidations before Actor delete = %d, want 3", beforeDelete)
	}

	if _, err := p.DeleteActor(ctx, resources.ActorRefFromActor(deleting)); err != nil {
		t.Fatalf("DeleteActor failed: %v", err)
	}
	var afterDelete, policies int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM effective_egress_policy_outbox`).Scan(&afterDelete); err != nil {
		t.Fatalf("counting invalidations after Actor delete: %v", err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM actor_egress_policies`).Scan(&policies); err != nil {
		t.Fatalf("counting policies after Actor delete: %v", err)
	}
	if afterDelete != beforeDelete+1 || policies != 0 {
		t.Fatalf("Actor delete left %d invalidations and %d policies, want %d and 0", afterDelete, policies, beforeDelete+1)
	}
}
