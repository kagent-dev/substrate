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

package imagecache

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
)

// Chain consults keychains in order and offers every credential they return,
// so a pull can try, say, the node's credential provider plugin and then the
// mounted pull Secrets. Nil keychains are dropped; with none left the result is
// nil (anonymous pulls), and a single keychain is returned as is.
func Chain(keychains ...authn.Keychain) authn.Keychain {
	var kcs []authn.Keychain
	for _, kc := range keychains {
		if kc != nil {
			kcs = append(kcs, kc)
		}
	}
	switch len(kcs) {
	case 0:
		return nil
	case 1:
		return kcs[0]
	}
	return &chain{keychains: kcs}
}

type chain struct {
	keychains []authn.Keychain
}

var _ CandidateKeychain = (*chain)(nil)

// Resolve implements authn.Keychain.
func (c *chain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return c.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain: the first candidate, or
// authn.Anonymous.
func (c *chain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	candidates, err := c.Candidates(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return authn.Anonymous, nil
	}
	return candidates[0], nil
}

// Candidates implements CandidateKeychain. A keychain's error ends the
// resolution: the pull fails with that error rather than with a misleading 401.
func (c *chain) Candidates(ctx context.Context, target authn.Resource) ([]authn.Authenticator, error) {
	var all []authn.Authenticator
	for _, kc := range c.keychains {
		candidates, err := candidatesOf(ctx, kc, target)
		if err != nil {
			return nil, err
		}
		all = append(all, candidates...)
	}
	return all, nil
}

// candidatesOf returns every credential kc offers for target: all of them for
// a CandidateKeychain, the single resolution otherwise. authn.Anonymous is not
// a candidate; the pull tries it on its own once the credentials run out.
func candidatesOf(ctx context.Context, kc authn.Keychain, target authn.Resource) ([]authn.Authenticator, error) {
	switch kc := kc.(type) {
	case nil:
		return nil, nil
	case CandidateKeychain:
		return kc.Candidates(ctx, target)
	default:
		auth, err := authn.Resolve(ctx, kc, target)
		if err != nil {
			return nil, err
		}
		if auth == authn.Anonymous {
			return nil, nil
		}
		return []authn.Authenticator{auth}, nil
	}
}
