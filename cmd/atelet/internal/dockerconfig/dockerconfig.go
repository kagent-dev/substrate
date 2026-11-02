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

// Package dockerconfig resolves registry credentials from Docker config files:
// a config.json, or the .dockerconfigjson key of a kubernetes.io/dockerconfigjson
// Secret, which is what a mounted image pull Secret presents.
//
// Actors are not Pods, so a Pod's imagePullSecrets never reach the images atelet
// pulls for them. Mounting the same Secrets into atelet and naming the files here
// gives actors the credentials a Pod in the namespace would have, with no plugin
// binary and no cloud SDK. Files are read on every resolution, so a Secret the
// kubelet refreshes in place takes effect without restarting atelet.
package dockerconfig

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// Keychain is a go-containerregistry keychain backed by Docker config files.
// It is safe for concurrent use.
type Keychain struct {
	paths []string
}

var (
	_ authn.Keychain        = (*Keychain)(nil)
	_ authn.ContextKeychain = (*Keychain)(nil)
)

// New returns a Keychain over the given files, consulted in order. Each file
// must load once now: a path that is wrong is a deployment mistake better
// reported at startup than as a 401 on the first pull.
func New(paths ...string) (*Keychain, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one Docker config file is required")
	}
	for _, p := range paths {
		if _, err := load(p); err != nil {
			return nil, err
		}
	}
	slog.Info("Loaded image pull Docker configs", slog.Any("paths", paths))
	return &Keychain{paths: paths}, nil
}

// Resolve implements authn.Keychain.
func (k *Keychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain. It returns the first
// candidate, or authn.Anonymous if there are none.
func (k *Keychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	candidates, err := k.Candidates(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return authn.Anonymous, nil
	}
	return candidates[0], nil
}

// Candidates returns one credential per file that has an entry for target, in
// file order, without duplicates. The lookup is the Docker CLI's: the exact
// repository string first, then the registry host, with Docker Hub mapped to its
// legacy auth key. A file that fails to load is logged and skipped so that one
// bad Secret does not take the others down with it.
func (k *Keychain) Candidates(ctx context.Context, target authn.Resource) ([]authn.Authenticator, error) {
	var candidates []authn.Authenticator
	seen := make(map[authn.AuthConfig]struct{})
	for _, p := range k.paths {
		cf, err := load(p)
		if err != nil {
			slog.WarnContext(ctx, "Skipping unreadable image pull Docker config",
				slog.String("path", p), slog.Any("err", err))
			continue
		}
		cfg, err := lookup(cf, target)
		if err != nil {
			slog.WarnContext(ctx, "Skipping image pull Docker config that failed to resolve",
				slog.String("path", p), slog.String("image", target.String()), slog.Any("err", err))
			continue
		}
		if cfg == nil {
			continue
		}
		if _, dup := seen[*cfg]; dup {
			continue
		}
		seen[*cfg] = struct{}{}
		candidates = append(candidates, authn.FromConfig(*cfg))
	}
	return candidates, nil
}

// load parses one Docker config file.
func load(path string) (*configfile.ConfigFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("while opening Docker config: %w", err)
	}
	defer f.Close()
	cf, err := config.LoadFromReader(f)
	if err != nil {
		return nil, fmt.Errorf("while parsing Docker config %q: %w", path, err)
	}
	return cf, nil
}

// lookup finds the entry for target in cf, or nil. It mirrors
// authn.DefaultKeychain so a config.json behaves exactly as it does for the
// Docker CLI and for ko.
func lookup(cf *configfile.ConfigFile, target authn.Resource) (*authn.AuthConfig, error) {
	var empty types.AuthConfig
	for _, key := range []string{target.String(), target.RegistryStr()} {
		if key == name.DefaultRegistry {
			key = authn.DefaultAuthKey
		}
		cfg, err := cf.GetAuthConfig(key)
		if err != nil {
			return nil, err
		}
		// GetAuthConfig fills ServerAddress; drop it for the emptiness test.
		cfg.ServerAddress = ""
		if cfg == empty {
			continue
		}
		return &authn.AuthConfig{
			Username:      cfg.Username,
			Password:      cfg.Password,
			Auth:          cfg.Auth,
			IdentityToken: cfg.IdentityToken,
			RegistryToken: cfg.RegistryToken,
		}, nil
	}
	return nil, nil
}
