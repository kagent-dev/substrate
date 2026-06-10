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
	vmMemoryBytes = 64 * 1024 * 1024 // 64 MiB — keeps VmRestore under ~50ms
	vmVcpus       = 2
	rootfsTag     = "rootfs"

	chReadyTimeout    = 15 * time.Second
	vfReadyTimeout    = 10 * time.Second
	goldenReadyTimeout = 15 * time.Second
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

// startVirtiofsd launches virtiofsd sharing sharedDir over sockPath.
// Uses context.Background() so it outlives the gRPC request context.
func startVirtiofsd(sockPath, sharedDir string) (*exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir for virtiofsd sock: %w", err)
	}
	_ = os.Remove(sockPath)

	cmd := exec.CommandContext(context.Background(), virtiofsdBin,
		"--socket-path", sockPath,
		"--shared-dir", sharedDir,
		"--cache", "auto",
		"--sandbox", "none",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting virtiofsd: %w", err)
	}

	deadline := time.Now().Add(vfReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("virtiofsd socket %q did not appear: %w", sockPath, err)
	}
	return cmd, nil
}

// startCloudHypervisor launches the cloud-hypervisor process and waits for its
// API socket to appear, then returns a connected client and the process handle.
// Uses context.Background() so it outlives the gRPC request context.
func startCloudHypervisor(sockPath string) (*ch.Client, *exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir for ch sock: %w", err)
	}
	_ = os.Remove(sockPath)

	cmd := exec.CommandContext(context.Background(), cloudHypervisorBin,
		"--api-socket", sockPath,
		"--log-file", "/dev/stderr",
		"-v",
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
	return ch.NewClient(sockPath), cmd, nil
}

// startedVM holds the handles to a ready virtiofsd+CH pair.
type startedVM struct {
	vfCmd    *exec.Cmd
	chClient *ch.Client
	chCmd    *exec.Cmd
}

// prewarmCHState holds a pre-started cloud-hypervisor process, ready to receive
// VmCreate or VmRestore without the ~50ms startup overhead on the critical path.
type prewarmCHState struct {
	client *ch.Client
	cmd    *exec.Cmd
}

// warmUp pre-warms a cloud-hypervisor process so RunWorkload doesn't pay CH
// startup latency (~50ms) on the first call. Called once before gRPC starts.
func (s *AteomService) warmUp(ctx context.Context) {
	slog.InfoContext(ctx, "Pre-warming cloud-hypervisor")
	client, cmd, err := startCloudHypervisor(chSockPath(s.podUID))
	if err != nil {
		slog.WarnContext(ctx, "CH prewarm failed, first call will be slower", slog.Any("err", err))
		return
	}
	s.prewarm = &prewarmCHState{client: client, cmd: cmd}
	slog.InfoContext(ctx, "Cloud-hypervisor pre-warmed and ready")
}

// doPrewarm starts a background CH process and stores it in s.prewarm.
// If s.prewarm is already set when the goroutine acquires the lock, the newly
// started CH is discarded (another goroutine beat us).
func (s *AteomService) doPrewarm() {
	slog.Info("Pre-warming cloud-hypervisor in background")
	client, cmd, err := startCloudHypervisor(chSockPath(s.podUID))
	if err != nil {
		slog.Warn("Background CH prewarm failed", slog.Any("err", err))
		return
	}
	s.lock.Lock()
	if s.prewarm != nil {
		_ = cmd.Process.Kill() // lost the race; discard
	} else {
		s.prewarm = &prewarmCHState{client: client, cmd: cmd}
		slog.Info("Background cloud-hypervisor pre-warm ready")
	}
	s.lock.Unlock()
}

// acquireVM returns a ready VM context: virtiofsd serving rootfsPath, a CH
// process with an open API socket, and a tap device. If a pre-warmed CH is
// available it is consumed (saving ~50ms); otherwise CH and virtiofsd start
// in parallel.
//
// On success the caller owns all returned resources and must clean them up.
// On error, returned processes (if any) have already been killed.
func (s *AteomService) acquireVM(ctx context.Context, rootfsPath string) (*startedVM, error) {
	vfsock := virtiofsdSockPath(s.podUID)

	pw := s.prewarm
	s.prewarm = nil

	if pw != nil {
		// Fast path: CH already running. Start virtiofsd and tap concurrently.
		type vfr struct {
			cmd *exec.Cmd
			err error
		}
		vfCh := make(chan vfr, 1)
		go func() {
			cmd, err := startVirtiofsd(vfsock, rootfsPath)
			vfCh <- vfr{cmd, err}
		}()
		_, tapErr := createTap(ctx, tapActorID)
		r := <-vfCh

		if tapErr != nil || r.err != nil {
			if r.cmd != nil {
				_ = r.cmd.Process.Kill()
			}
			_ = pw.cmd.Process.Kill()
			if tapErr == nil {
				_ = deleteTap(ctx, tapActorID)
			}
			if tapErr != nil {
				return nil, tapErr
			}
			return nil, r.err
		}
		return &startedVM{vfCmd: r.cmd, chClient: pw.client, chCmd: pw.cmd}, nil
	}

	// Slow path: start CH and virtiofsd in parallel with the tap.
	chSock := chSockPath(s.podUID)
	type vfResult struct {
		cmd *exec.Cmd
		err error
	}
	type chResult struct {
		client *ch.Client
		cmd    *exec.Cmd
		err    error
	}

	vfCh := make(chan vfResult, 1)
	chCh := make(chan chResult, 1)
	go func() {
		cmd, err := startVirtiofsd(vfsock, rootfsPath)
		vfCh <- vfResult{cmd, err}
	}()
	go func() {
		client, cmd, err := startCloudHypervisor(chSock)
		chCh <- chResult{client, cmd, err}
	}()

	_, tapErr := createTap(ctx, tapActorID)

	vf := <-vfCh
	chR := <-chCh

	if tapErr != nil || vf.err != nil || chR.err != nil {
		if vf.cmd != nil {
			_ = vf.cmd.Process.Kill()
		}
		if chR.cmd != nil {
			_ = chR.cmd.Process.Kill()
		}
		if tapErr == nil {
			_ = deleteTap(ctx, tapActorID)
		}
		if tapErr != nil {
			return nil, tapErr
		}
		if vf.err != nil {
			return nil, fmt.Errorf("virtiofsd: %w", vf.err)
		}
		return nil, fmt.Errorf("cloud-hypervisor: %w", chR.err)
	}
	return &startedVM{vfCmd: vf.cmd, chClient: chR.client, chCmd: chR.cmd}, nil
}

// prepareRootfsForGolden ensures the actor rootfs contains golden-init and has
// a clean state for golden snapshot restore:
//   - copies golden-init so `init=/golden-init` is valid on cold boot
//   - creates .ateom-ready so virtiofsd DEVICE_STATE can reopen it
//   - removes stale .ateom-run-args so goldeninit doesn't exec old args
func prepareRootfsForGolden(rootfsPath string) error {
	data, err := os.ReadFile(goldenInitBin)
	if err != nil {
		return fmt.Errorf("reading golden-init binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsPath, "golden-init"), data, 0o755); err != nil {
		return fmt.Errorf("writing golden-init to rootfs: %w", err)
	}
	// Pre-create .ateom-ready so virtiofsd can reopen it at restore time
	// (the golden snapshot captures virtiofsd state while this file exists).
	if err := os.WriteFile(filepath.Join(rootfsPath, readyFileName), []byte("ok\n"), 0o644); err != nil {
		return fmt.Errorf("writing .ateom-ready: %w", err)
	}
	// Remove any stale args so goldeninit doesn't exec old entrypoint.
	_ = os.Remove(filepath.Join(rootfsPath, argsFileName))
	return nil
}

// waitForGoldenReady polls for goldeninit's .ateom-ready marker in the host
// rootfs. virtiofsd propagates the guest write to the host within ~1ms.
func waitForGoldenReady(rootfsPath string) error {
	readyPath := filepath.Join(rootfsPath, readyFileName)
	deadline := time.Now().Add(goldenReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("goldeninit did not signal ready within %s", goldenReadyTimeout)
}

// RunWorkload boots a microVM for the actor.
//
// Fast path (template golden snapshot available): VmRestore from the golden
// snapshot for this template, then write .ateom-run-args for goldeninit to
// exec the workload. Eliminates kernel boot time (~80ms) after the first call.
//
// Slow path (first RunWorkload for this template on this pod): cold-boots with
// init=/golden-init, waits for goldeninit to signal ready, takes a golden
// snapshot (pause+snapshot+resume, ~25ms), then writes .ateom-run-args.
// Subsequent calls use the fast path.
//
// In both paths a pre-warmed CH is consumed if available, saving ~50ms.
func (s *AteomService) RunWorkload(ctx context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	s.actorLogger.EmitLifecycleLog("Actor starting", id, tmpl, ns)

	containers := req.GetSpec().GetContainers()
	if len(containers) == 0 {
		return nil, fmt.Errorf("workload spec has no containers")
	}
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

	if err := os.MkdirAll(actorRuntimeDir(s.podUID, id), 0o700); err != nil {
		return nil, fmt.Errorf("creating actor runtime dir: %w", err)
	}

	if err := prepareRootfsForGolden(rootfsPath); err != nil {
		return nil, fmt.Errorf("preparing rootfs: %w", err)
	}

	vfsock := virtiofsdSockPath(s.podUID)
	templateGolden := goldenSnapshotForTemplate(s.podUID, ns, tmpl, primaryContainer)

	if _, statErr := os.Stat(templateGolden); statErr == nil {
		// ── Fast path: restore from template golden snapshot ─────────────
		slog.InfoContext(ctx, "RunWorkload: golden restore", slog.String("golden", templateGolden))

		vm, err := s.acquireVM(ctx, rootfsPath)
		if err != nil {
			return nil, fmt.Errorf("acquiring VM: %w", err)
		}
		restoreURL := "file://" + templateGolden
		if err := vm.chClient.VmRestore(ctx, ch.RestoreConfig{SourceURL: restoreURL}); err != nil {
			_ = vm.chCmd.Process.Kill()
			_ = vm.vfCmd.Process.Kill()
			_ = deleteTap(ctx, tapActorID)
			return nil, fmt.Errorf("CH VmRestore (golden): %w", err)
		}
		if err := vm.chClient.VmResume(ctx); err != nil {
			_ = vm.chCmd.Process.Kill()
			_ = vm.vfCmd.Process.Kill()
			_ = deleteTap(ctx, tapActorID)
			return nil, fmt.Errorf("CH VmResume (golden): %w", err)
		}
		// Write args after resume; virtiofsd is live so goldeninit sees it
		// within its 10ms poll interval.
		argsPath := filepath.Join(rootfsPath, argsFileName)
		if err := os.WriteFile(argsPath, []byte(strings.Join(entrypoint, "\n")), 0o644); err != nil {
			_ = vm.chCmd.Process.Kill()
			_ = vm.vfCmd.Process.Kill()
			_ = deleteTap(ctx, tapActorID)
			return nil, fmt.Errorf("writing run args: %w", err)
		}

		s.running = &runningActor{
			chCmd: vm.chCmd, vfCmd: vm.vfCmd, chClient: vm.chClient,
			tapActorID: tapActorID, vfsockPath: vfsock,
		}
		go s.doPrewarm()
		s.actorLogger.EmitLifecycleLog("Actor started", id, tmpl, ns)
		return &ateompb.RunWorkloadResponse{}, nil
	}

	// ── Slow path: cold boot, capture golden snapshot, then run ──────────
	slog.InfoContext(ctx, "RunWorkload: cold boot (first for template, will snapshot)", slog.String("template", ns+":"+tmpl))

	// Remove the pre-created .ateom-ready so we can detect when goldeninit
	// actually boots and rewrites it.
	_ = os.Remove(filepath.Join(rootfsPath, readyFileName))

	vm, err := s.acquireVM(ctx, rootfsPath)
	if err != nil {
		return nil, fmt.Errorf("acquiring VM: %w", err)
	}
	killVM := func() {
		_ = vm.chCmd.Process.Kill()
		_ = vm.vfCmd.Process.Kill()
		_ = deleteTap(ctx, tapActorID)
	}

	vmCfg := ch.VmConfig{
		Payload: &ch.PayloadConfig{
			Kernel:  guestKernel,
			Cmdline: "root=rootfs rootfstype=virtiofs rw console=ttyS0 init=/golden-init",
		},
		Memory:  &ch.MemoryConfig{Size: vmMemoryBytes, Shared: true},
		Cpus:    &ch.CpusConfig{BootVcpus: vmVcpus, MaxVcpus: vmVcpus},
		Fs:      []ch.FsConfig{{Tag: rootfsTag, Socket: vfsock, NumQueues: 1, QueueSize: 1024}},
		Net:     []ch.NetConfig{{Tap: tapName(tapActorID), Mtu: tapMTU}},
		Serial:  &ch.SerialConfig{Mode: "File", File: "/dev/stdout"},
		Console: &ch.SerialConfig{Mode: "Null"},
	}
	if err := vm.chClient.VmCreate(ctx, vmCfg); err != nil {
		killVM()
		return nil, fmt.Errorf("CH VmCreate: %w", err)
	}
	if err := vm.chClient.VmBoot(ctx); err != nil {
		killVM()
		return nil, fmt.Errorf("CH VmBoot: %w", err)
	}

	// Wait for goldeninit to signal it's running.
	if err := waitForGoldenReady(rootfsPath); err != nil {
		killVM()
		return nil, fmt.Errorf("waiting for goldeninit: %w", err)
	}

	// Snapshot the idle VM as the template golden.
	if err := os.MkdirAll(templateGolden, 0o700); err != nil {
		killVM()
		return nil, fmt.Errorf("mkdir template golden: %w", err)
	}
	if err := vm.chClient.VmPause(ctx); err != nil {
		killVM()
		return nil, fmt.Errorf("CH VmPause (golden): %w", err)
	}
	snapURL := "file://" + templateGolden
	if err := vm.chClient.VmSnapshot(ctx, ch.SnapshotConfig{DestinationURL: snapURL}); err != nil {
		_ = vm.chClient.VmResume(ctx)
		killVM()
		return nil, fmt.Errorf("CH VmSnapshot (golden): %w", err)
	}
	if err := vm.chClient.VmResume(ctx); err != nil {
		killVM()
		return nil, fmt.Errorf("CH VmResume (golden): %w", err)
	}
	slog.InfoContext(ctx, "Template golden snapshot saved", slog.String("dir", templateGolden))

	// Write args now that the VM is live; goldeninit exec's within ~10ms.
	argsPath := filepath.Join(rootfsPath, argsFileName)
	if err := os.WriteFile(argsPath, []byte(strings.Join(entrypoint, "\n")), 0o644); err != nil {
		killVM()
		return nil, fmt.Errorf("writing run args: %w", err)
	}

	s.running = &runningActor{
		chCmd: vm.chCmd, vfCmd: vm.vfCmd, chClient: vm.chClient,
		tapActorID: tapActorID, vfsockPath: vfsock,
	}
	go s.doPrewarm()
	s.actorLogger.EmitLifecycleLog("Actor started", id, tmpl, ns)
	return &ateompb.RunWorkloadResponse{}, nil
}

// CheckpointWorkload snapshots the running VM then tears everything down.
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
	// CH's VmSnapshot fails with EEXIST if any output files are already present.
	if err := os.RemoveAll(checkpointDir); err != nil {
		return nil, fmt.Errorf("clearing checkpoint dir: %w", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating checkpoint dir: %w", err)
	}

	if err := r.chClient.VmPause(ctx); err != nil {
		return nil, fmt.Errorf("CH VmPause: %w", err)
	}
	snapshotURL := "file://" + checkpointDir
	if err := r.chClient.VmSnapshot(ctx, ch.SnapshotConfig{DestinationURL: snapshotURL}); err != nil {
		_ = r.chClient.VmResume(ctx)
		return nil, fmt.Errorf("CH VmSnapshot: %w", err)
	}

	s.teardown(ctx, r)
	s.running = nil

	s.actorLogger.EmitLifecycleLog("Actor checkpointed", id, tmpl, ns)
	return &ateompb.CheckpointWorkloadResponse{}, nil
}

// RestoreWorkload restores a VM from a snapshot previously written by CheckpointWorkload.
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

	vfsock := virtiofsdSockPath(s.podUID)
	vm, err := s.acquireVM(ctx, rootfsPath)
	if err != nil {
		return nil, fmt.Errorf("acquiring VM: %w", err)
	}

	restoreURL := "file://" + restoreDir
	if err := vm.chClient.VmRestore(ctx, ch.RestoreConfig{SourceURL: restoreURL}); err != nil {
		_ = vm.chCmd.Process.Kill()
		_ = vm.vfCmd.Process.Kill()
		_ = deleteTap(ctx, tapActorID)
		return nil, fmt.Errorf("CH VmRestore: %w", err)
	}
	if err := vm.chClient.VmResume(ctx); err != nil {
		_ = vm.chCmd.Process.Kill()
		_ = vm.vfCmd.Process.Kill()
		_ = deleteTap(ctx, tapActorID)
		return nil, fmt.Errorf("CH VmResume: %w", err)
	}

	s.running = &runningActor{
		chCmd: vm.chCmd, vfCmd: vm.vfCmd, chClient: vm.chClient,
		tapActorID: tapActorID, vfsockPath: vfsock,
	}

	go s.doPrewarm()

	s.actorLogger.EmitLifecycleLog("Actor restored", id, tmpl, ns)
	return &ateompb.RestoreWorkloadResponse{}, nil
}

// teardown kills CH and virtiofsd and removes the tap device and vf socket.
// Must be called while holding s.lock (it starts a doPrewarm goroutine that
// will wait for the lock before writing to s.prewarm).
func (s *AteomService) teardown(ctx context.Context, r *runningActor) {
	_ = r.chClient.VmShutdown(ctx)
	time.Sleep(500 * time.Millisecond)
	if r.chCmd.ProcessState == nil {
		_ = r.chCmd.Process.Kill()
	}
	if r.vfCmd != nil && r.vfCmd.ProcessState == nil {
		_ = r.vfCmd.Process.Kill()
	}
	_ = deleteTap(ctx, r.tapActorID)
	if r.vfsockPath != "" {
		_ = os.Remove(r.vfsockPath)
	}
	// Start next pre-warm. The goroutine blocks on s.lock until the current
	// RPC handler releases it, then fills s.prewarm for the next operation.
	go s.doPrewarm()
}
