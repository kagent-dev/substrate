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
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/jackc/pgx/v5"
)

const pollEffectiveEgressPolicyOutboxSQL = `
	SELECT xid::text AS xid_text, atespace, actor_name
	FROM effective_egress_policy_outbox
	WHERE xid > $1::xid8
	  AND xid < pg_snapshot_xmin(pg_current_snapshot())
	ORDER BY xid LIMIT $2`

const pollEffectiveEgressPolicySafetySQL = `
	SELECT EXISTS(
		SELECT 1 FROM effective_egress_policy_outbox_trim
		WHERE xid > $1::xid8 AND xid > $2::xid8),
	pg_postmaster_start_time()::text`

// writeAndAppendEffectiveEgressPolicyChange commits a mutation and its Actor-key
// invalidation atomically. Each mutation must append at most one row because the
// watch cursor orders rows by transaction ID alone.
func (p *Persistence) writeAndAppendEffectiveEgressPolicyChange(ctx context.Context, actorRef resources.ActorRef, fn func(context.Context, pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO effective_egress_policy_outbox (atespace, actor_name)
		VALUES ($1, $2)`, actorRef.Atespace, actorRef.Name); err != nil {
		return fmt.Errorf("appending effective egress policy invalidation for %s: %w", actorRef, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// WatchEffectiveEgressPolicyChanges polls committed Actor-key invalidations in
// transaction order. A closed channel means the consumer must establish a new
// watch; unlike the Worker cache, it never requires a full Actor relist.
func (p *Persistence) WatchEffectiveEgressPolicyChanges(ctx context.Context) (*store.EffectiveEgressPolicyWatch, error) {
	watchCtx, cancel := context.WithCancel(ctx)

	var cursorXid, baselineXid, baselineStart string
	if err := p.watchPool.QueryRow(watchCtx, `
		SELECT (pg_snapshot_xmin(pg_current_snapshot())::text::numeric - 1)::text,
		       GREATEST(
		           COALESCE((SELECT max(xid) FROM effective_egress_policy_outbox
		                     WHERE xid < pg_snapshot_xmin(pg_current_snapshot())), '0'::xid8),
		           COALESCE((SELECT xid FROM effective_egress_policy_outbox_trim), '0'::xid8))::text,
		       pg_postmaster_start_time()::text`).Scan(&cursorXid, &baselineXid, &baselineStart); err != nil {
		cancel()
		return nil, fmt.Errorf("reading effective egress policy outbox cursor: %w", err)
	}

	ch := make(chan store.EffectiveEgressPolicyChange, 128)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(outboxPollInterval)
		defer ticker.Stop()
		var failingSince time.Time
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}
			for {
				batch := &pgx.Batch{}
				batch.Queue(pollEffectiveEgressPolicyOutboxSQL, cursorXid, outboxBatch)
				batch.Queue(pollEffectiveEgressPolicySafetySQL, cursorXid, baselineXid)
				results := p.watchPool.SendBatch(watchCtx, batch)

				type feedRow struct {
					xid      string
					atespace string
					name     string
				}
				var rowsBatch []feedRow
				rows, err := results.Query()
				if err == nil {
					for rows.Next() {
						var row feedRow
						if err = rows.Scan(&row.xid, &row.atespace, &row.name); err != nil {
							rowsBatch = nil
							break
						}
						rowsBatch = append(rowsBatch, row)
					}
					rows.Close()
				}
				var fellBehind bool
				var postmasterStart string
				if err == nil {
					err = results.QueryRow().Scan(&fellBehind, &postmasterStart)
				}
				if closeErr := results.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					if watchCtx.Err() != nil {
						return
					}
					if failingSince.IsZero() {
						failingSince = time.Now()
					} else if time.Since(failingSince) > p.pollFailureCloseAfter {
						slog.WarnContext(watchCtx, "effective egress policy outbox polling has failed persistently; closing watch",
							slog.Duration("failing_for", time.Since(failingSince)), slog.Any("err", err))
						return
					}
					slog.WarnContext(watchCtx, "effective egress policy outbox poll failed", slog.Any("err", err))
					break
				}
				failingSince = time.Time{}
				if postmasterStart != baselineStart {
					slog.WarnContext(watchCtx, "database restarted under the effective egress policy outbox; closing watch for resync")
					return
				}
				if fellBehind {
					slog.WarnContext(watchCtx, "effective egress policy watch fell behind outbox retention; closing for resync",
						slog.String("cursor_xid", cursorXid))
					return
				}

				for _, row := range rowsBatch {
					change := store.EffectiveEgressPolicyChange{Actor: resources.ActorRef{Atespace: row.atespace, Name: row.name}}
					select {
					case ch <- change:
						cursorXid = row.xid
					case <-watchCtx.Done():
						return
					}
				}
				if len(rowsBatch) < outboxBatch {
					break
				}
			}
		}
	}()
	return store.NewEffectiveEgressPolicyWatch(ch, cancel), nil
}
