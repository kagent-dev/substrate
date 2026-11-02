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
	"errors"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// resolveOnly is a plain authn.Keychain (no Candidates) returning one auth.
type resolveOnly struct {
	auth authn.Authenticator
	err  error
}

func (r resolveOnly) Resolve(authn.Resource) (authn.Authenticator, error) { return r.auth, r.err }

// candidatesOnly is a CandidateKeychain returning several auths.
type candidatesOnly struct{ auths []authn.Authenticator }

func (c candidatesOnly) Resolve(authn.Resource) (authn.Authenticator, error) {
	return c.auths[0], nil
}

func (c candidatesOnly) Candidates(context.Context, authn.Resource) ([]authn.Authenticator, error) {
	return c.auths, nil
}

func basic(user string) authn.Authenticator {
	return authn.FromConfig(authn.AuthConfig{Username: user, Password: "p"})
}

func usernames(t *testing.T, auths []authn.Authenticator) []string {
	t.Helper()
	var out []string
	for _, a := range auths {
		cfg, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cfg.Username)
	}
	return out
}

func TestChain(t *testing.T) {
	target, err := name.NewRepository("registry.example.com/app")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if Chain() != nil || Chain(nil, nil) != nil {
		t.Error("Chain of nothing should be nil (anonymous pulls)")
	}
	single := resolveOnly{auth: basic("only")}
	if got := Chain(nil, single); got != single {
		t.Errorf("Chain of one keychain should return it unchanged, got %T", got)
	}

	kc := Chain(
		candidatesOnly{auths: []authn.Authenticator{basic("plugin-a"), basic("plugin-b")}},
		resolveOnly{auth: authn.Anonymous}, // contributes nothing
		resolveOnly{auth: basic("secret")},
	)
	ck, ok := kc.(CandidateKeychain)
	if !ok {
		t.Fatalf("Chain of several keychains is a %T, want CandidateKeychain", kc)
	}
	got, err := ck.Candidates(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if u := usernames(t, got); len(u) != 3 || u[0] != "plugin-a" || u[1] != "plugin-b" || u[2] != "secret" {
		t.Errorf("candidates = %v, want [plugin-a plugin-b secret]", u)
	}
	first, err := kc.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	if u := usernames(t, []authn.Authenticator{first}); u[0] != "plugin-a" {
		t.Errorf("Resolve = %v, want the first candidate", u)
	}

	empty := Chain(resolveOnly{auth: authn.Anonymous}, resolveOnly{auth: authn.Anonymous})
	if a, err := empty.Resolve(target); err != nil || a != authn.Anonymous {
		t.Errorf("Resolve with no credentials = %v, %v; want Anonymous", a, err)
	}

	boom := errors.New("boom")
	failing := Chain(resolveOnly{auth: basic("x")}, resolveOnly{err: boom})
	if _, err := failing.(CandidateKeychain).Candidates(ctx, target); !errors.Is(err, boom) {
		t.Errorf("a failing keychain should fail the chain, got %v", err)
	}
}
