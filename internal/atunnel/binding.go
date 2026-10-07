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

package atunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
)

const (
	// CredentialKeyHeader carries an opaque lookup handle for an input's
	// credential binding. The handle is not the credential itself.
	CredentialKeyHeader = "X-Ate-Credential-Key"
	// BindingSequenceHeader orders credential bindings for one actor UID.
	BindingSequenceHeader = "X-Ate-Binding-Seq"
	// ActorUIDHeader identifies the actor incarnation selected by the router.
	ActorUIDHeader = "X-Ate-Actor-Uid"
)

var errBindingConflict = errors.New("atunnel: stale or conflicting credential binding")

// parseCredentialBinding leaves keyless control requests without a binding change.
func parseCredentialBinding(h http.Header) (uid, key string, sequence uint64, err error) {
	keys := h.Values(CredentialKeyHeader)
	if len(keys) == 0 {
		return "", "", 0, nil
	}
	uids := h.Values(ActorUIDHeader)
	sequences := h.Values(BindingSequenceHeader)
	if len(keys) != 1 || len(uids) != 1 || uids[0] == "" || len(sequences) != 1 {
		return "", "", 0, errors.New("credential binding requires one key, one actor UID and one sequence")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(keys[0])
	if err != nil || len(decoded) != 32 || len(keys[0]) != 43 {
		return "", "", 0, errors.New("credential key must encode 32 bytes as unpadded base64url")
	}
	sequence, err = strconv.ParseUint(sequences[0], 10, 64)
	if err != nil || sequence == 0 {
		return "", "", 0, errors.New("binding sequence must be a positive decimal uint64")
	}
	return uids[0], keys[0], sequence, nil
}

// A binding owns the connections admitted under its immutable key and sequence.
// Its activation's bindingMu protects the connection set and connection fields.
type credentialBinding struct {
	key      string
	sequence uint64
	ctx      context.Context
	cancel   context.CancelFunc
	conns    map[*egressConnection]struct{}
}

func newCredentialBinding(parent context.Context, key string, sequence uint64) *credentialBinding {
	ctx, cancel := context.WithCancel(parent)
	return &credentialBinding{key: key, sequence: sequence, ctx: ctx, cancel: cancel, conns: make(map[*egressConnection]struct{})}
}

func (b *credentialBinding) close() {
	b.cancel()
	for conn := range b.conns {
		conn.close()
	}
}

type egressConnection struct {
	downstream net.Conn
	upstream   net.Conn
}

func (c *egressConnection) close() {
	_ = c.downstream.Close()
	if c.upstream != nil {
		_ = c.upstream.Close()
	}
}
