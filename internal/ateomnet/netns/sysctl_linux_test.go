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

package netns

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/roottest"
	"golang.org/x/sys/unix"
)

// Only EROFS takes the remount path: any other error is reported as it is,
// and /proc/sys is left as it was found. Remounting it read-only on the way
// out would break every later write.
func TestSetSysctlReportsAnUnrelatedError(t *testing.T) {
	err := setSysctl("net/ipv4/ateomnet_no_such_sysctl", "0")
	if !errors.Is(err, unix.ENOENT) {
		t.Fatalf("setSysctl() on a missing key: got %v, want ENOENT", err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/proc/sys", &st); err != nil {
		t.Fatalf("statfs /proc/sys: %v", err)
	}
	if st.Flags&unix.ST_RDONLY != 0 {
		t.Error("/proc/sys was left read-only")
	}
}

// Concurrent callers share one /proc/sys mount; none may fail because another
// restored it read-only before its write.
func TestSetSysctlSerializesTheRemount(t *testing.T) {
	const callers = 16
	fake := &fakeProcSys{readOnly: true, values: map[string]string{}}
	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			errs[i] = fake.procSys().set(fmt.Sprintf("net/ipv4/caller_%d", i), "0")
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if fake.maxRemounted != 1 {
		t.Errorf("%d callers had /proc/sys remounted read-write at once, want 1", fake.maxRemounted)
	}
	if !fake.readOnly {
		t.Error("/proc/sys was left read-write")
	}
}

// fakeProcSys is a read-only /proc/sys mount shared by all callers. Its
// read-write remount is slow, so unserialized callers overlap inside it.
type fakeProcSys struct {
	mu       sync.Mutex
	readOnly bool
	values   map[string]string
	// remounted counts callers inside the read-write window; maxRemounted is
	// its peak.
	remounted, maxRemounted int
}

func (f *fakeProcSys) procSys() procSys {
	return procSys{
		readFile: func(path string) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if v, ok := f.values[path]; ok {
				return []byte(v + "\n"), nil
			}
			return []byte("1024\n"), nil
		},
		writeFile: func(path string, data []byte) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.readOnly {
				return &os.PathError{Op: "open", Path: path, Err: unix.EROFS}
			}
			f.values[path] = strings.TrimSpace(string(data))
			return nil
		},
		remount: func(readOnly bool) error {
			if readOnly {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.readOnly = true
				f.remounted--
				return nil
			}
			time.Sleep(time.Millisecond)
			f.mu.Lock()
			f.readOnly = false
			f.remounted++
			f.maxRemounted = max(f.maxRemounted, f.remounted)
			f.mu.Unlock()
			time.Sleep(time.Millisecond)
			return nil
		},
	}
}

func TestSetSysctlConcurrent(t *testing.T) {
	roottest.Require(t, "creates mount and network namespaces")
	const helperEnv = "SUBSTRATE_TEST_SYSCTL_HELPER"
	if os.Getenv(helperEnv) != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// Keep the host's mounts and network sysctls untouched.
		cmd := exec.Command(binary, "-test.run=^TestSetSysctlConcurrent$", "-test.v")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWNET}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated sysctl test: %v\n%s", err, out)
		}
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("/proc/sys", "/proc/sys", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("none", "/proc/sys", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		t.Fatal(err)
	}

	// Actor setups share a mount namespace but write sysctls in separate
	// network namespaces. Every writer must finish before /proc/sys goes RO.
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	for range 16 {
		ready.Add(1)
		done.Go(func() {
			runtime.LockOSThread()
			// Leave the thread locked so it is destroyed with its private netns.
			err := unix.Unshare(unix.CLONE_NEWNET)
			ready.Done()
			if err != nil {
				t.Error(err)
				return
			}
			<-start
			for range 64 {
				for _, value := range []string{"0", "1024"} {
					if err := setSysctl("net/ipv4/ip_unprivileged_port_start", value); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	ready.Wait()
	close(start)
	done.Wait()
	var st unix.Statfs_t
	if err := unix.Statfs("/proc/sys", &st); err != nil {
		t.Fatal(err)
	}
	if st.Flags&unix.ST_RDONLY == 0 {
		t.Error("/proc/sys was left writable")
	}
}
