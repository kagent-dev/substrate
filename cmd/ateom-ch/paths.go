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
	goldenInitBin      = "/usr/local/share/ateom-ch/golden-init"

	// argsFileName is written to the workload rootfs after golden restore so
	// goldeninit (PID 1) can exec the actual workload entrypoint.
	argsFileName = ".ateom-run-args"

	// readyFileName is created by goldeninit inside the VM to signal that
	// PID 1 is live. warmUpTemplate polls this file before snapshotting.
	readyFileName = ".ateom-ready"

	// basePath is the host directory mounted into the ateom-ch container,
	// shared with atelet (which writes OCI bundles here via ateompath).
	basePath = "/var/lib/ateom-gvisor"

	// runtimeBasePath holds per-pod cloud-hypervisor API sockets and virtiofsd
	// sockets. Kept on tmpfs (/run) so it survives only for the pod lifetime.
	runtimeBasePath = "/run/ateom-ch"

	// tapActorID is used to derive the tap and bridge names for all VMs.
	// A fixed ID means the tap name is always "atch-ch0", which matches the
	// device name stored in every actor checkpoint snapshot.
	tapActorID = "ch0"
)

// chSockPath returns the cloud-hypervisor API unix socket for this pod.
// The pod UID is included so multiple ateom-ch pods on the same host node
// don't collide (though each pod has its own /run mount via emptyDir).
func chSockPath(podUID string) string {
	return filepath.Join(runtimeBasePath, podUID, "ch.sock")
}

// virtiofsdSockPath returns the virtiofsd vhost-user socket.
//
// IMPORTANT: this path is embedded verbatim in every CH snapshot (in the fs
// device config). Restoring a snapshot requires virtiofsd to be listening at
// exactly this path. Using a fixed path (not pod-UID-specific) means a
// snapshot taken on pod A can be restored on pod B without path mismatch —
// each pod gets its own isolated /run/ateom-ch/ mount via emptyDir, so there
// is no cross-pod socket collision.
func virtiofsdSockPath(_ string) string {
	return filepath.Join(runtimeBasePath, "virtiofsd.sock")
}

// actorRuntimeDir is the runtime directory for a running actor.
func actorRuntimeDir(podUID, actorID string) string {
	return filepath.Join(runtimeBasePath, podUID, actorID)
}

// goldenSnapshotForTemplate returns the directory where the per-template golden
// VM snapshot is stored. The snapshot is taken once (first RunWorkload for the
// template on this pod) and reused for all subsequent RunWorkloads of the same
// template, replacing kernel boot with a fast VmRestore.
func goldenSnapshotForTemplate(podUID, ns, tmpl, container string) string {
	return filepath.Join(runtimeBasePath, podUID, "golden-"+ns+"-"+tmpl+"-"+container)
}
