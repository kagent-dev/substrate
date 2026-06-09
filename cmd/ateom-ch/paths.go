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

import "path/filepath"

const (
	// Binaries baked into the ateom-ch container image.
	cloudHypervisorBin = "/usr/local/bin/cloud-hypervisor"
	virtiofsdBin       = "/usr/local/bin/virtiofsd"
	guestKernel        = "/usr/local/share/ateom-ch/vmlinux"

	// basePath is the host directory mounted into the ateom-ch container,
	// shared with atelet (which writes OCI bundles here via ateompath).
	//
	// We intentionally reuse ateompath.BasePath ("/var/lib/ateom-gvisor") so
	// atelet's existing bundle-preparation logic needs no changes for phase 2.
	basePath = "/var/lib/ateom-gvisor"

	// runtimeBasePath holds per-pod cloud-hypervisor API sockets and virtiofsd
	// sockets.  Kept on tmpfs (/run) so it survives only for the pod lifetime.
	runtimeBasePath = "/run/ateom-ch"
)

// chSockPath returns the cloud-hypervisor API unix socket for this pod.
func chSockPath(podUID string) string {
	return filepath.Join(runtimeBasePath, podUID, "ch.sock")
}

// chPidFile returns the pidfile for the cloud-hypervisor process.
func chPidFile(podUID string) string {
	return filepath.Join(runtimeBasePath, podUID, "ch.pid")
}

// virtiofsdSockPath returns the virtiofsd socket for a given actor+container.
func virtiofsdSockPath(podUID, actorID, containerName string) string {
	return filepath.Join(runtimeBasePath, podUID, actorID+"-"+containerName+".virtiofsd.sock")
}

// actorRuntimeDir is the runtime directory for a running actor.
func actorRuntimeDir(podUID, actorID string) string {
	return filepath.Join(runtimeBasePath, podUID, actorID)
}
