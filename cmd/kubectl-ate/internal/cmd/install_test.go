// Copyright 2026 The Agent Substrate Authors
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

package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestLocalFlagSet(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "none", args: nil, want: false},
		{name: "local", args: []string{"--microvm"}, want: true},
		{name: "persistent only", args: []string{"--context", "kind"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "root"}
			root.PersistentFlags().String("context", "", "")
			var got bool
			sub := &cobra.Command{Use: "install", RunE: func(cmd *cobra.Command, _ []string) error {
				got = localFlagSet(cmd)
				return nil
			}}
			sub.Flags().Bool("microvm", false, "")
			root.AddCommand(sub)
			root.SetArgs(append([]string{"install"}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("localFlagSet() = %v, want %v", got, tc.want)
			}
		})
	}
}
