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

// Package ch provides a client for the Cloud Hypervisor REST API.
package ch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Client talks to a single cloud-hypervisor process over its Unix socket API.
type Client struct {
	hc     *http.Client
	baseURL string
}

// NewClient returns a Client connected to the cloud-hypervisor API socket at sockPath.
func NewClient(sockPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{
		hc:      &http.Client{Transport: transport},
		baseURL: "http://localhost/api/v1",
	}
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CH %s %s: status %d: %s", http.MethodPut, path, resp.StatusCode, b)
	}
	return nil
}

// --- API types ---

type PayloadConfig struct {
	Kernel  string `json:"kernel"`
	Cmdline string `json:"cmdline,omitempty"`
}

type MemoryConfig struct {
	Size   uint64 `json:"size"`
	Shared bool   `json:"shared,omitempty"`
}

type CpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

// FsConfig wires a virtiofsd socket into the VM as a virtio-fs device.
type FsConfig struct {
	Tag       string `json:"tag"`
	Socket    string `json:"socket"`
	NumQueues int    `json:"num_queues"`
	QueueSize int    `json:"queue_size"`
}

// NetConfig attaches a tap device to the VM as a virtio-net device.
type NetConfig struct {
	Tap string `json:"tap"`
	Mtu uint16 `json:"mtu,omitempty"`
}

type SerialConfig struct {
	Mode string `json:"mode"` // "Null", "Tty", "File", "Off"
	File string `json:"file,omitempty"`
}

type VmConfig struct {
	Payload *PayloadConfig `json:"payload"`
	Memory  *MemoryConfig  `json:"memory,omitempty"`
	Cpus    *CpusConfig    `json:"cpus,omitempty"`
	Fs      []FsConfig     `json:"fs,omitempty"`
	Net     []NetConfig    `json:"net,omitempty"`
	Serial  *SerialConfig  `json:"serial,omitempty"`
	Console *SerialConfig  `json:"console,omitempty"`
}

type SnapshotConfig struct {
	// DestinationURL is a local path (file://) or URL where the snapshot is written.
	DestinationURL string `json:"destination_url"`
}

type RestoreConfig struct {
	// SourceURL is the directory the snapshot was written to.
	SourceURL string `json:"source_url"`
	Prefault  bool   `json:"prefault"`
}

// --- API calls ---

func (c *Client) VmCreate(ctx context.Context, cfg VmConfig) error {
	return c.put(ctx, "/vm.create", cfg)
}

func (c *Client) VmBoot(ctx context.Context) error {
	return c.put(ctx, "/vm.boot", nil)
}

func (c *Client) VmPause(ctx context.Context) error {
	return c.put(ctx, "/vm.pause", nil)
}

func (c *Client) VmResume(ctx context.Context) error {
	return c.put(ctx, "/vm.resume", nil)
}

func (c *Client) VmSnapshot(ctx context.Context, cfg SnapshotConfig) error {
	return c.put(ctx, "/vm.snapshot", cfg)
}

func (c *Client) VmRestore(ctx context.Context, cfg RestoreConfig) error {
	return c.put(ctx, "/vm.restore", cfg)
}

func (c *Client) VmShutdown(ctx context.Context) error {
	return c.put(ctx, "/vm.shutdown", nil)
}
