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

package cmd

import (
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/cmd/kubectl-ate/internal/printer"
	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
)

var workerPoolGrantAtespace string

var adminWorkerPoolCmd = &cobra.Command{
	Use:     "workerpool",
	Aliases: []string{"worker-pool", "wp"},
	Short:   "Manage worker pool access",
}

var adminWorkerPoolGrantCmd = &cobra.Command{
	Use:   "grant namespace/name --atespace atespace",
	Short: "Grant an atespace access to a worker pool",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if workerPoolGrantAtespace == "" {
			return fmt.Errorf("--atespace is required")
		}
		namespace, name, err := parseWorkerPoolRef(args[0])
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		resp, err := apiClient.CreateWorkerPoolGrant(ctx, &ateapipb.CreateWorkerPoolGrantRequest{
			Grant: &ateapipb.WorkerPoolGrant{
				Atespace: workerPoolGrantAtespace,
				WorkerPool: &ateapipb.WorkerPoolRef{
					Namespace: namespace,
					Name:      name,
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to grant worker pool access: %w", err)
		}
		return printer.PrintWorkerPoolGrant(resp.GetGrant(), outputFmt)
	},
}

var adminWorkerPoolRevokeCmd = &cobra.Command{
	Use:   "revoke namespace/name --atespace atespace",
	Short: "Revoke an atespace's access to a worker pool",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if workerPoolGrantAtespace == "" {
			return fmt.Errorf("--atespace is required")
		}
		namespace, name, err := parseWorkerPoolRef(args[0])
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		if _, err := apiClient.DeleteWorkerPoolGrant(ctx, &ateapipb.DeleteWorkerPoolGrantRequest{
			Atespace: workerPoolGrantAtespace,
			WorkerPool: &ateapipb.WorkerPoolRef{
				Namespace: namespace,
				Name:      name,
			},
		}); err != nil {
			return fmt.Errorf("failed to revoke worker pool access: %w", err)
		}
		fmt.Printf("workerpool access revoked for atespace %q on %s/%s\n", workerPoolGrantAtespace, namespace, name)
		return nil
	},
}

var adminWorkerPoolGrantsCmd = &cobra.Command{
	Use:   "grants",
	Short: "List worker pool access grants",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		resp, err := apiClient.ListWorkerPoolGrants(ctx, &ateapipb.ListWorkerPoolGrantsRequest{
			Atespace: workerPoolGrantAtespace,
		})
		if err != nil {
			return fmt.Errorf("failed to list worker pool access grants: %w", err)
		}
		return printer.PrintWorkerPoolGrants(resp.GetGrants(), outputFmt)
	},
}

func parseWorkerPoolRef(arg string) (string, string, error) {
	namespace, name, ok := strings.Cut(arg, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("worker pool must be specified as namespace/name")
	}
	return namespace, name, nil
}

func init() {
	adminWorkerPoolGrantCmd.Flags().StringVarP(&workerPoolGrantAtespace, "atespace", "a", "", "Atespace to grant access to")
	adminWorkerPoolRevokeCmd.Flags().StringVarP(&workerPoolGrantAtespace, "atespace", "a", "", "Atespace to revoke access from")
	adminWorkerPoolGrantsCmd.Flags().StringVarP(&workerPoolGrantAtespace, "atespace", "a", "", "Only list grants for this atespace")

	adminWorkerPoolCmd.AddCommand(adminWorkerPoolGrantCmd)
	adminWorkerPoolCmd.AddCommand(adminWorkerPoolRevokeCmd)
	adminWorkerPoolCmd.AddCommand(adminWorkerPoolGrantsCmd)
	adminCmd.AddCommand(adminWorkerPoolCmd)
}
