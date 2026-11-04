// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"testing"

	"go.ate.dev/substrate/cmd/ate-setup/internal/config"
)

func TestPublishSourceRepository(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "upstream", want: "https://github.com/agent-substrate/substrate"},
		{name: "fork", env: map[string]string{"ATE_PUBLISH_SOURCE_URL": "https://github.com/kagent-dev/substrate"}, want: "https://github.com/kagent-dev/substrate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := config.Resolve(nil, config.ResolveOptions{Env: tc.env})
			if err != nil {
				t.Fatal(err)
			}
			if got := r.String("publish.sourceURL"); got != tc.want {
				t.Errorf("publish.sourceURL = %q, want %q", got, tc.want)
			}
		})
	}
}
