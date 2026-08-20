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

package controlapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/ateredis"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAcquireActorLockWorkflowDeadline(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = rdb.Close() })
	w := &ActorWorkflow{store: ateredis.NewPersistence(rdb), workflowDeadline: 20 * time.Millisecond}

	ctx, lock, err := w.acquireActorLock(context.Background(), resources.ActorRef{Atespace: "space", Name: "actor"})
	if err != nil {
		t.Fatalf("acquireActorLock: %v", err)
	}
	t.Cleanup(lock.Close)

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("workflow context did not reach its deadline")
	}
}
