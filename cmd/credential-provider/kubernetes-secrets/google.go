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

// This file serves the google-access-token.kubernetes.io provider name. Google
// APIs accept OAuth 2.0 access tokens rather than service account keys, and
// minting a token means signing a JWT with the key. Doing that here keeps the
// key out of the gateway and out of actors, which receive a token that expires
// within the hour instead.

package main

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// GoogleAccessTokenProviderName is the ate-secret:// URI host that resolves a
	// Secret entry holding a Google service account key to an access token for
	// that service account. The path is <namespace>/<secret>[/<key>], as for
	// ProviderName.
	GoogleAccessTokenProviderName = "google-access-token.kubernetes.io"

	// googleCloudPlatformScope is the scope of every minted token. It is the one
	// Vertex AI accepts, so the service account's IAM roles decide what the
	// token may do.
	googleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

	// googleTokenMinRemaining is the lifetime a token must have left to be handed
	// out. The gateway caches a fetched credential for five minutes per actor,
	// so a token returned with less than that would be used after it expired.
	// Fifteen minutes leaves margin over that cache and still exchanges a key
	// once every 45 minutes of an hour-long token.
	googleTokenMinRemaining = 15 * time.Minute

	// googleTokenExchangeTimeout bounds one round trip to the token endpoint.
	googleTokenExchangeTimeout = 15 * time.Second

	// googleErrorMessageLimit caps how much of a token endpoint's error response
	// is repeated in a status message.
	googleErrorMessageLimit = 200
)

// googleTokenExchanger mints Google access tokens from service account keys
// and caches each token, keyed by the key's contents, until it nears expiry.
type googleTokenExchanger struct {
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	tokens map[[sha256.Size]byte]*googleToken
}

// googleToken is one cached access token. Its lock serializes exchanges for a
// key, so actors starting together share one round trip to Google.
type googleToken struct {
	mu     sync.Mutex
	value  string
	expiry time.Time
}

// newGoogleTokenExchanger builds an exchanger that reaches token endpoints
// through client.
func newGoogleTokenExchanger(client *http.Client) *googleTokenExchanger {
	return &googleTokenExchanger{client: client, now: time.Now, tokens: map[[sha256.Size]byte]*googleToken{}}
}

// AccessToken returns a bearer token for the service account key, serving the
// cached token while it has at least googleTokenMinRemaining left. Errors are
// gRPC statuses: FailedPrecondition when the key is unusable or Google rejects
// it, Unavailable when the endpoint cannot be reached or misbehaves.
func (g *googleTokenExchanger) AccessToken(ctx context.Context, key []byte) (string, error) {
	conf, err := parseGoogleServiceAccountKey(key)
	if err != nil {
		return "", status.Error(codes.FailedPrecondition, err.Error())
	}
	entry := g.entry(sha256.Sum256(key))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.value != "" && entry.expiry.Sub(g.now()) >= googleTokenMinRemaining {
		return entry.value, nil
	}
	token, err := g.exchange(ctx, conf)
	if err != nil {
		return "", err
	}
	entry.value, entry.expiry = token.AccessToken, token.Expiry
	return entry.value, nil
}

// entry returns the cache slot for a key. Expired entries are dropped on the
// way so rotated keys do not accumulate; TryLock skips one another goroutine
// is refreshing, since deleting it would let a third start a duplicate
// exchange.
func (g *googleTokenExchanger) entry(sum [sha256.Size]byte) *googleToken {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, t := range g.tokens {
		if k == sum || !t.mu.TryLock() {
			continue
		}
		if t.value != "" && !t.expiry.After(now) {
			delete(g.tokens, k)
		}
		t.mu.Unlock()
	}
	t, ok := g.tokens[sum]
	if !ok {
		t = &googleToken{}
		g.tokens[sum] = t
	}
	return t
}

// exchange performs the JWT bearer grant. A 4xx from the endpoint means the
// key is not accepted, which retrying will not fix; anything else is transient.
func (g *googleTokenExchanger) exchange(ctx context.Context, conf *jwt.Config) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTokenExchangeTimeout)
	defer cancel()
	token, err := conf.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, g.client)).Token()
	if err != nil {
		var rejected *oauth2.RetrieveError
		if errors.As(err, &rejected) && rejected.Response != nil && rejected.Response.StatusCode >= 400 && rejected.Response.StatusCode < 500 {
			reason := oauthErrorMessage(rejected.Body)
			slog.WarnContext(ctx, "Google rejected the service account key",
				slog.String("service_account", conf.Email), slog.Int("status", rejected.Response.StatusCode), slog.String("reason", reason))
			return nil, status.Errorf(codes.FailedPrecondition, "Google rejected the service account key for %s: %s", conf.Email, reason)
		}
		slog.WarnContext(ctx, "Google token exchange failed", slog.String("service_account", conf.Email), slog.Any("err", err))
		return nil, status.Error(codes.Unavailable, "could not exchange the service account key for an access token")
	}
	// The lifetime is read from the response rather than the library's Expiry so
	// that one clock, g.now, decides both when a token was issued and when it is
	// too old to hand out.
	seconds, _ := token.Extra("expires_in").(float64)
	if token.AccessToken == "" || seconds <= 0 {
		return nil, status.Error(codes.Unavailable, "token endpoint returned no access token or no expiry")
	}
	lifetime := time.Duration(seconds * float64(time.Second))
	if lifetime < googleTokenMinRemaining {
		return nil, status.Errorf(codes.Unavailable, "token endpoint issued a token valid for %s; at least %s is required to outlive the gateway cache", lifetime.Round(time.Second), googleTokenMinRemaining)
	}
	token.Expiry = g.now().Add(lifetime)
	slog.InfoContext(ctx, "exchanged a Google service account key for an access token",
		slog.String("service_account", conf.Email), slog.Time("expires_at", token.Expiry))
	return token, nil
}

// parseGoogleServiceAccountKey validates a key file well enough that a failure
// names the Secret's problem instead of surfacing as an opaque exchange error.
// The key's own token_uri is honored, as Google's libraries do, but it must be
// HTTPS: the assertion is signed and the response is a bearer token.
func parseGoogleServiceAccountKey(key []byte) (*jwt.Config, error) {
	conf, err := google.JWTConfigFromJSON(key, googleCloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("secret is not a Google service account key: %v", err)
	}
	if conf.Email == "" {
		return nil, errors.New("service account key has no client_email")
	}
	if err := validateRSAPrivateKey(conf.PrivateKey); err != nil {
		return nil, fmt.Errorf("service account key private_key %v", err)
	}
	u, err := url.Parse(conf.TokenURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("service account key token_uri %q must be an https URL", conf.TokenURL)
	}
	return conf, nil
}

// validateRSAPrivateKey checks that the key parses as the RSA key the JWT
// bearer grant signs with, in either PEM encoding Google issues.
func validateRSAPrivateKey(pemKey []byte) error {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return errors.New("is not PEM encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return errors.New("is not a PKCS#1 or PKCS#8 private key")
		}
	}
	if _, ok := parsed.(*rsa.PrivateKey); !ok {
		return errors.New("is not an RSA key")
	}
	return nil
}

// oauthErrorMessage reduces a token endpoint error response to its RFC 6749
// error and description, bounded so the status message stays readable.
func oauthErrorMessage(body []byte) string {
	var fields struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &fields) != nil || fields.Error == "" {
		return "unrecognized error response"
	}
	message := fields.Error
	if fields.Description != "" {
		message += ": " + fields.Description
	}
	if len(message) > googleErrorMessageLimit {
		message = strings.ToValidUTF8(message[:googleErrorMessageLimit], "")
	}
	return message
}
