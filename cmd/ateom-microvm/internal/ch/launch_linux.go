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

package ch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/vishvananda/netns"
)

// LaunchVMMOptions configures starting a bare VMM (no VM) for boot or restore.
type LaunchVMMOptions struct {
	// Binary is the cloud-hypervisor executable (defaults to "cloud-hypervisor").
	Binary string
	// APISocket is the api-socket path the new VMM should listen on.
	APISocket string
	// NetNS is the required network namespace containing the VM's TAP.
	NetNS netns.NsHandle
	// File-backed output avoids starting exec copier goroutines while in NetNS.
	// Nil sends output to the null device.
	Stdout, Stderr *os.File
}

// LaunchVMM starts a cloud-hypervisor process with only an api-socket (no VM)
// and waits until it answers. Use Client.RestoreWithNetFDs to then restore a
// snapshot that has fd-backed net devices. The caller owns cmd.
func LaunchVMM(ctx context.Context, o LaunchVMMOptions) (*exec.Cmd, *Client, error) {
	if o.APISocket == "" {
		return nil, nil, fmt.Errorf("LaunchVMMOptions.APISocket is required")
	}
	bin := o.Binary
	if bin == "" {
		bin = "cloud-hypervisor"
	}
	_ = os.Remove(o.APISocket)
	// Deliberately NOT exec.CommandContext: the VMM must outlive the RPC whose
	// ctx launched it. The caller owns cmd; WaitReady honors ctx.
	cmd := exec.Command(bin, "--api-socket", o.APISocket)
	if o.Stdout != nil {
		cmd.Stdout = o.Stdout
	}
	if o.Stderr != nil {
		cmd.Stderr = o.Stderr
	}
	// The child inherits the creating thread's namespace and stays there when
	// ateom restores its own thread. This puts CH's TAP operations in Gateway;
	// it does not enter the VM's guest kernel. Only cmd.Start runs inside NetNS,
	// so readiness polling and its goroutines run after restoration.
	if err := ateomnet.NetNSDo(ctx, o.NetNS, func(context.Context) error {
		return cmd.Start()
	}); err != nil {
		return nil, nil, fmt.Errorf("while starting cloud-hypervisor: %w", err)
	}
	client := NewClient(o.APISocket)
	if _, err := client.WaitReady(ctx, 15*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, nil, fmt.Errorf("while waiting for VMM api-socket: %w", err)
	}
	return cmd, client, nil
}
