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

// goldeninit is PID 1 of the per-template golden VM snapshot used by ateom-ch.
//
// On first boot (slow path, snapshot creation): signals readiness by creating
// /.ateom-ready, then idles until ateom-ch snapshots and resumes the VM.
//
// After resume (any subsequent RunWorkload for the same template): reads
// /.ateom-run-args written by ateom-ch and exec's the actual workload
// entrypoint, becoming the workload's PID 1.
package main

import (
	"os"
	"strings"
	"syscall"
	"time"
)

func main() {
	_ = os.WriteFile("/.ateom-ready", []byte("ok\n"), 0o644)

	for {
		data, err := os.ReadFile("/.ateom-run-args")
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 && lines[0] != "" {
				_ = syscall.Exec(lines[0], lines, os.Environ())
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}
