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
	"encoding/base64"
	"errors"
	"net/http"
)

const (
	// CredentialKeyHeader carries an opaque lookup handle for an input's
	// credential binding. The handle is not the credential itself.
	CredentialKeyHeader = "X-Ate-Credential-Key"
	// ActorUIDHeader identifies the actor incarnation selected by the router.
	ActorUIDHeader = "X-Ate-Actor-Uid"
)

// parseCredentialKey leaves keyless control requests without a binding change.
func parseCredentialKey(h http.Header) (uid, key string, err error) {
	keys := h.Values(CredentialKeyHeader)
	if len(keys) == 0 {
		return "", "", nil
	}
	uids := h.Values(ActorUIDHeader)
	if len(keys) != 1 || len(uids) != 1 || uids[0] == "" {
		return "", "", errors.New("credential binding requires one key and one actor UID")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(keys[0])
	if err != nil || len(decoded) != 32 || len(keys[0]) != 43 {
		return "", "", errors.New("credential key must encode 32 bytes as unpadded base64url")
	}
	return uids[0], keys[0], nil
}
