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

package ateredis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const egressUpdateMaxAttempts = 5

func (s *Persistence) GetEgressPolicy(ctx context.Context, atespace, name string) (*ateapipb.EgressPolicy, error) {
	dbKey := egressPolicyDBKey(atespace, name)
	b, err := s.rdb.Get(ctx, dbKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("while getting egress policy key %q: %w", dbKey, err)
	}
	policy := &ateapipb.EgressPolicy{}
	if err := protojson.Unmarshal(b, policy); err != nil {
		return nil, fmt.Errorf("while unmarshaling egress policy: %w", err)
	}
	return policy, nil
}

func (s *Persistence) GetEgressPolicyForActor(ctx context.Context, actorRef resources.ActorRef, actorUID string) (*ateapipb.EgressPolicy, error) {
	b, err := s.rdb.Get(ctx, egressPolicyActorIndexDBKey(actorRef)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("while resolving actor egress policy: %w", err)
	}
	var idx egressPolicyActorIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("while decoding actor egress policy index: %w", err)
	}
	if idx.ActorUID != actorUID {
		return nil, store.ErrUIDConflict
	}
	return s.GetEgressPolicy(ctx, actorRef.Atespace, idx.PolicyName)
}

func (s *Persistence) CreateEgressPolicy(ctx context.Context, policy *ateapipb.EgressPolicy, actorUID string) (*ateapipb.EgressPolicy, error) {
	dbPolicy := proto.Clone(policy).(*ateapipb.EgressPolicy)
	dbPolicy.Metadata = newCreateMetadata(policy.GetMetadata().GetAtespace(), policy.GetMetadata().GetName())
	policyBytes, err := protojson.Marshal(dbPolicy)
	if err != nil {
		return nil, fmt.Errorf("while marshaling egress policy: %w", err)
	}
	idxBytes, err := json.Marshal(egressPolicyActorIndex{PolicyName: dbPolicy.GetMetadata().GetName(), ActorUID: actorUID})
	if err != nil {
		return nil, fmt.Errorf("while marshaling actor egress policy index: %w", err)
	}
	policyKey := egressPolicyDBKey(dbPolicy.GetMetadata().GetAtespace(), dbPolicy.GetMetadata().GetName())
	actorRef := resources.ActorRefFromObjectRef(dbPolicy.GetActor())
	indexKey := egressPolicyActorIndexDBKey(actorRef)

	for range egressUpdateMaxAttempts {
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			n, err := tx.Exists(ctx, policyKey, indexKey).Result()
			if err != nil {
				return err
			}
			if n != 0 {
				return store.ErrAlreadyExists
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, policyKey, policyBytes, 0)
				pipe.Set(ctx, indexKey, idxBytes, 0)
				return nil
			})
			return err
		}, policyKey, indexKey)
		switch {
		case err == nil:
			return dbPolicy, nil
		case errors.Is(err, store.ErrAlreadyExists):
			return nil, store.ErrAlreadyExists
		case errors.Is(err, redis.TxFailedErr):
			continue
		default:
			return nil, fmt.Errorf("while creating egress policy: %w", err)
		}
	}
	return nil, store.ErrVersionConflict
}

func (s *Persistence) UpdateEgressPolicy(ctx context.Context, atespace, name string, mutate func(*ateapipb.EgressPolicy) error) (*ateapipb.EgressPolicy, error) {
	dbKey := egressPolicyDBKey(atespace, name)
	for range egressUpdateMaxAttempts {
		var dbPolicy *ateapipb.EgressPolicy
		var abortErr error
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			currentVal, err := tx.Get(ctx, dbKey).Bytes()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					return store.ErrNotFound
				}
				return fmt.Errorf("while getting egress policy: %w", err)
			}
			current := &ateapipb.EgressPolicy{}
			if err := protojson.Unmarshal(currentVal, current); err != nil {
				return fmt.Errorf("while unmarshaling egress policy: %w", err)
			}
			before := proto.Clone(current).(*ateapipb.EgressPolicy)
			if err := mutate(current); err != nil {
				abortErr = err
				return err
			}
			if !proto.Equal(before.GetActor(), current.GetActor()) {
				abortErr = store.ErrFailedPrecondition
				return abortErr
			}
			current.Metadata = newUpdateMetadata(before.GetMetadata())
			newVal, err := protojson.Marshal(current)
			if err != nil {
				return fmt.Errorf("while marshaling egress policy: %w", err)
			}
			if _, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, dbKey, newVal, 0)
				return nil
			}); err != nil {
				return err
			}
			dbPolicy = current
			return nil
		}, dbKey)

		switch {
		case err == nil:
			return dbPolicy, nil
		case abortErr != nil:
			return nil, abortErr
		case errors.Is(err, store.ErrNotFound):
			return nil, store.ErrNotFound
		case errors.Is(err, redis.TxFailedErr):
			continue
		default:
			return nil, fmt.Errorf("while executing update egress policy transaction: %w", err)
		}
	}
	return nil, store.ErrVersionConflict
}

func (s *Persistence) DeleteEgressPolicy(ctx context.Context, atespace, name string) (*ateapipb.EgressPolicy, error) {
	policy, err := s.GetEgressPolicy(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	policyKey := egressPolicyDBKey(atespace, name)
	indexKey := egressPolicyActorIndexDBKey(resources.ActorRefFromObjectRef(policy.GetActor()))
	if err := s.rdb.Del(ctx, policyKey, indexKey).Err(); err != nil {
		return nil, fmt.Errorf("while deleting egress policy: %w", err)
	}
	return policy, nil
}

func (s *Persistence) ListEgressPolicies(ctx context.Context, atespace string, pageSize int32, pageToken string) ([]*ateapipb.EgressPolicy, string, error) {
	var result []*ateapipb.EgressPolicy
	next, err := s.listPage(ctx, egressPolicyScanPattern(atespace), pageSize, pageToken, func(ctx context.Context, master *redis.Client, keys []string) (int, error) {
		policies, err := fetchProtos(ctx, master, keys, func() *ateapipb.EgressPolicy { return &ateapipb.EgressPolicy{} })
		if err != nil {
			return 0, err
		}
		result = append(result, policies...)
		return len(policies), nil
	})
	if err != nil {
		return nil, "", err
	}
	return result, next, nil
}

func (s *Persistence) GetCredential(ctx context.Context, atespace, name string) (*ateapipb.Credential, error) {
	dbKey := credentialDBKey(atespace, name)
	b, err := s.rdb.Get(ctx, dbKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("while getting credential key %q: %w", dbKey, err)
	}
	credential := &ateapipb.Credential{}
	if err := protojson.Unmarshal(b, credential); err != nil {
		return nil, fmt.Errorf("while unmarshaling credential: %w", err)
	}
	return credential, nil
}

func (s *Persistence) CreateCredential(ctx context.Context, credential *ateapipb.Credential) (*ateapipb.Credential, error) {
	dbCredential := proto.Clone(credential).(*ateapipb.Credential)
	dbCredential.Metadata = newCreateMetadata(credential.GetMetadata().GetAtespace(), credential.GetMetadata().GetName())
	b, err := protojson.Marshal(dbCredential)
	if err != nil {
		return nil, fmt.Errorf("while marshaling credential: %w", err)
	}
	created, err := s.rdb.SetNX(ctx, credentialDBKey(dbCredential.GetMetadata().GetAtespace(), dbCredential.GetMetadata().GetName()), b, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("while creating credential: %w", err)
	}
	if !created {
		return nil, store.ErrAlreadyExists
	}
	return dbCredential, nil
}

func (s *Persistence) UpdateCredential(ctx context.Context, atespace, name string, mutate func(*ateapipb.Credential) error) (*ateapipb.Credential, error) {
	dbKey := credentialDBKey(atespace, name)
	for range egressUpdateMaxAttempts {
		var dbCredential *ateapipb.Credential
		var abortErr error
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			currentVal, err := tx.Get(ctx, dbKey).Bytes()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					return store.ErrNotFound
				}
				return fmt.Errorf("while getting credential: %w", err)
			}
			current := &ateapipb.Credential{}
			if err := protojson.Unmarshal(currentVal, current); err != nil {
				return fmt.Errorf("while unmarshaling credential: %w", err)
			}
			before := proto.Clone(current).(*ateapipb.Credential)
			if err := mutate(current); err != nil {
				abortErr = err
				return err
			}
			current.Metadata = newUpdateMetadata(before.GetMetadata())
			newVal, err := protojson.Marshal(current)
			if err != nil {
				return fmt.Errorf("while marshaling credential: %w", err)
			}
			if _, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, dbKey, newVal, 0)
				return nil
			}); err != nil {
				return err
			}
			dbCredential = current
			return nil
		}, dbKey)

		switch {
		case err == nil:
			return dbCredential, nil
		case abortErr != nil:
			return nil, abortErr
		case errors.Is(err, store.ErrNotFound):
			return nil, store.ErrNotFound
		case errors.Is(err, redis.TxFailedErr):
			continue
		default:
			return nil, fmt.Errorf("while executing update credential transaction: %w", err)
		}
	}
	return nil, store.ErrVersionConflict
}

func (s *Persistence) DeleteCredential(ctx context.Context, atespace, name string) (*ateapipb.Credential, error) {
	credential, err := s.GetCredential(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	if err := s.rdb.Del(ctx, credentialDBKey(atespace, name)).Err(); err != nil {
		return nil, fmt.Errorf("while deleting credential: %w", err)
	}
	return credential, nil
}

func (s *Persistence) ListCredentials(ctx context.Context, atespace string, pageSize int32, pageToken string) ([]*ateapipb.Credential, string, error) {
	var result []*ateapipb.Credential
	next, err := s.listPage(ctx, credentialScanPattern(atespace), pageSize, pageToken, func(ctx context.Context, master *redis.Client, keys []string) (int, error) {
		credentials, err := fetchProtos(ctx, master, keys, func() *ateapipb.Credential { return &ateapipb.Credential{} })
		if err != nil {
			return 0, err
		}
		result = append(result, credentials...)
		return len(credentials), nil
	})
	if err != nil {
		return nil, "", err
	}
	return result, next, nil
}
