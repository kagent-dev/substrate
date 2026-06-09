//go:build linux

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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-ch/internal/ch"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

const (
	vmMemory  = "512M"
	vmVcpus   = 2
	// rootfs virtiofs tag — must match the kernel cmdline root= parameter.
	rootfsTag = "rootfs"

	// Timeout for cloud-hypervisor to expose its API socket after launch.
	chReadyTimeout = 15 * time.Second
)

// ociProcess is the subset of an OCI config.json process spec we need.
type ociProcess struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
}

type ociConfig struct {
	Process ociProcess `json:"process"`
}

func readOCIEntrypoint(bundlePath string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("reading config.json: %w", err)
	}
	var cfg ociConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config.json: %w", err)
	}
	return cfg.Process.Args, nil
}

// startVirtiofsd launches virtiofsd serving sharedDir on sockPath and returns
// the process (caller must eventually kill it).
func startVirtiofsd(ctx context.Context, sockPath, sharedDir string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, virtiofsdBin,
		"--socket-path", sockPath,
		"--shared-dir", sharedDir,
		"--cache", "auto",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting virtiofsd: %w", err)
	}
	// Give virtiofsd a moment to create the socket.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return cmd, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("virtiofsd socket %q did not appear within 5s", sockPath)
}

// startCloudHypervisor launches the cloud-hypervisor process and waits for its
// API socket to appear, then returns a connected client and the process handle.
func startCloudHypervisor(ctx context.Context, sockPath string) (*ch.Client, *exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir for ch sock: %w", err)
	}
	_ = os.Remove(sockPath)

	cmd := exec.CommandContext(ctx, cloudHypervisorBin,
		"--api-socket", sockPath,
		"--log-file", "/dev/stderr",
		"--v", "1",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting cloud-hypervisor: %w", err)
	}

	deadline := time.Now().Add(chReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		_ = cmd.Process.Kill()
		return nil, nil, fmt.Errorf("cloud-hypervisor API socket %q did not appear: %w", sockPath, err)
	}

	client := ch.NewClient(sockPath)
	return client, cmd, nil
}

// RunWorkload boots a new Cloud Hypervisor microVM for the actor.
//
// Atelet contract: OCI bundles are already written under ateompath.OCIBundlePath.
// For phase 2 we support a single app container; the pause container is not used.
func (s *AteomService) RunWorkload(ctx context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	s.actorLogger.EmitLifecycleLog("Actor starting", id, tmpl, ns)

	containers := req.GetSpec().GetContainers()
	if len(containers) == 0 {
		return nil, fmt.Errorf("workload spec has no containers")
	}
	// Phase 2: single-container. Use first container's bundle as VM rootfs.
	// TODO: multi-container support (additional virtiofs mounts + minimal init).
	primaryContainer := containers[0].GetName()

	bundlePath := ateompath.OCIBundlePath(ns, tmpl, id, primaryContainer)
	rootfsPath := filepath.Join(bundlePath, "rootfs")

	entrypoint, err := readOCIEntrypoint(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("reading OCI entrypoint: %w", err)
	}
	if len(entrypoint) == 0 {
		return nil, fmt.Errorf("OCI config.json has empty process.args")
	}

	// Create runtime dirs.
	if err := os.MkdirAll(actorRuntimeDir(s.podUID, id), 0o700); err != nil {
		return nil, fmt.Errorf("creating actor runtime dir: %w", err)
	}

	// Start virtiofsd for the rootfs.
	vfsockPath := virtiofsdSockPath(s.podUID, id, primaryContainer)
	vfCmd, err := startVirtiofsd(ctx, vfsockPath, rootfsPath)
	if err != nil {
		return nil, err
	}

	// Create tap device and bridge to eth0.
	tapDev, err := createTap(ctx, id)
	if err != nil {
		_ = vfCmd.Process.Kill()
		return nil, err
	}

	// Launch cloud-hypervisor.
	sockPath := chSockPath(s.podUID)
	client, chCmd, err := startCloudHypervisor(ctx, sockPath)
	if err != nil {
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, err
	}

	// Build kernel cmdline: boot directly into the OCI entrypoint.
	cmdline := buildKernelCmdline(entrypoint)
	slog.InfoContext(ctx, "Booting VM", slog.String("cmdline", cmdline))

	vmCfg := ch.VmConfig{
		Payload: &ch.PayloadConfig{
			Kernel:  guestKernel,
			Cmdline: cmdline,
		},
		Memory: &ch.MemoryConfig{Size: vmMemory},
		Cpus:   &ch.CpusConfig{BootVcpus: vmVcpus, MaxVcpus: vmVcpus},
		Fs: []ch.FsConfig{
			{Tag: rootfsTag, Socket: vfsockPath, NumQueues: 1, QueueSize: 1024},
		},
		Net:     []ch.NetConfig{{Tap: tapDev, Mtu: tapMTU}},
		Serial:  &ch.SerialConfig{Mode: "File", File: "/dev/stdout"},
		Console: &ch.SerialConfig{Mode: "Null"},
	}

	if err := client.VmCreate(ctx, vmCfg); err != nil {
		_ = chCmd.Process.Kill()
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, fmt.Errorf("CH VmCreate: %w", err)
	}
	if err := client.VmBoot(ctx); err != nil {
		_ = chCmd.Process.Kill()
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, fmt.Errorf("CH VmBoot: %w", err)
	}

	// Record running state so Checkpoint can clean up.
	s.running = &runningActor{
		chCmd:     chCmd,
		vfCmd:     vfCmd,
		chClient:  client,
		tapActorID: id,
		vfsockPath: vfsockPath,
	}

	s.actorLogger.EmitLifecycleLog("Actor started", id, tmpl, ns)
	return &ateompb.RunWorkloadResponse{}, nil
}

// CheckpointWorkload snapshots the running VM, then tears everything down.
//
// Atelet contract: after this returns, atelet uploads checkpoint-state/ to object storage.
func (s *AteomService) CheckpointWorkload(ctx context.Context, req *ateompb.CheckpointWorkloadRequest) (*ateompb.CheckpointWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	s.actorLogger.EmitLifecycleLog("Actor checkpointing", id, tmpl, ns)

	if s.running == nil {
		return nil, fmt.Errorf("no running actor to checkpoint")
	}
	r := s.running

	checkpointDir := ateompath.CheckpointStateDir(ns, tmpl, id)
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating checkpoint dir: %w", err)
	}

	if err := r.chClient.VmPause(ctx); err != nil {
		return nil, fmt.Errorf("CH VmPause: %w", err)
	}

	// cloud-hypervisor expects a file:// URL.
	snapshotURL := "file://" + checkpointDir
	if err := r.chClient.VmSnapshot(ctx, ch.SnapshotConfig{DestinationURL: snapshotURL}); err != nil {
		// Best-effort resume so the VM isn't stuck paused.
		_ = r.chClient.VmResume(ctx)
		return nil, fmt.Errorf("CH VmSnapshot: %w", err)
	}

	s.teardown(ctx, r)
	s.running = nil

	s.actorLogger.EmitLifecycleLog("Actor checkpointed", id, tmpl, ns)
	return &ateompb.CheckpointWorkloadResponse{}, nil
}

// RestoreWorkload restores a VM from a snapshot previously written by CheckpointWorkload.
//
// Atelet contract: snapshot already downloaded into ateompath.RestoreStateDir.
func (s *AteomService) RestoreWorkload(ctx context.Context, req *ateompb.RestoreWorkloadRequest) (*ateompb.RestoreWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	s.actorLogger.EmitLifecycleLog("Actor restoring", id, tmpl, ns)

	containers := req.GetSpec().GetContainers()
	if len(containers) == 0 {
		return nil, fmt.Errorf("workload spec has no containers")
	}
	primaryContainer := containers[0].GetName()

	restoreDir := ateompath.RestoreStateDir(ns, tmpl, id)
	bundlePath := ateompath.OCIBundlePath(ns, tmpl, id, primaryContainer)
	rootfsPath := filepath.Join(bundlePath, "rootfs")

	if err := os.MkdirAll(actorRuntimeDir(s.podUID, id), 0o700); err != nil {
		return nil, fmt.Errorf("creating actor runtime dir: %w", err)
	}

	// Restart virtiofsd for the rootfs (snapshot does not include filesystem state).
	vfsockPath := virtiofsdSockPath(s.podUID, id, primaryContainer)
	vfCmd, err := startVirtiofsd(ctx, vfsockPath, rootfsPath)
	if err != nil {
		return nil, err
	}

	if _, err := createTap(ctx, id); err != nil {
		_ = vfCmd.Process.Kill()
		return nil, err
	}

	sockPath := chSockPath(s.podUID)
	client, chCmd, err := startCloudHypervisor(ctx, sockPath)
	if err != nil {
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, err
	}

	restoreURL := "file://" + restoreDir
	if err := client.VmRestore(ctx, ch.RestoreConfig{SourceURL: restoreURL, Prefault: false}); err != nil {
		_ = chCmd.Process.Kill()
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, fmt.Errorf("CH VmRestore: %w", err)
	}
	if err := client.VmResume(ctx); err != nil {
		_ = chCmd.Process.Kill()
		_ = vfCmd.Process.Kill()
		_ = deleteTap(ctx, id)
		return nil, fmt.Errorf("CH VmResume: %w", err)
	}

	s.running = &runningActor{
		chCmd:      chCmd,
		vfCmd:      vfCmd,
		chClient:   client,
		tapActorID: id,
		vfsockPath: vfsockPath,
	}

	s.actorLogger.EmitLifecycleLog("Actor restored", id, tmpl, ns)
	return &ateompb.RestoreWorkloadResponse{}, nil
}

// teardown kills the CH process, virtiofsd, and removes the tap device.
func (s *AteomService) teardown(ctx context.Context, r *runningActor) {
	_ = r.chClient.VmShutdown(ctx)
	time.Sleep(500 * time.Millisecond)
	if r.chCmd.ProcessState == nil {
		_ = r.chCmd.Process.Kill()
	}
	_ = r.vfCmd.Process.Kill()
	_ = deleteTap(ctx, r.tapActorID)
	_ = os.Remove(r.vfsockPath)
}

// buildKernelCmdline constructs the Linux kernel cmdline for booting directly
// into the OCI process entrypoint from a virtiofs root.
func buildKernelCmdline(entrypoint []string) string {
	// The kernel mounts the virtiofs share tagged "rootfs" as /, then execs
	// the OCI entrypoint as PID 1.
	parts := []string{
		"root=" + rootfsTag,
		"rootfstype=virtiofs",
		"rw",
		"console=ttyS0",
		"init=" + entrypoint[0],
	}
	for _, arg := range entrypoint[1:] {
		parts = append(parts, "--", arg)
	}
	return strings.Join(parts, " ")
}
