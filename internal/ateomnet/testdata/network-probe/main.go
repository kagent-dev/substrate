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

// network-probe runs as an OCI application or a minimal VM init process.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "pause" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "guest" {
		for _, fs := range []string{"proc", "sysfs", "devtmpfs"} {
			path := map[string]string{"proc": "/proc", "sysfs": "/sys", "devtmpfs": "/dev"}[fs]
			must(os.MkdirAll(path, 0o755))
			must(unix.Mount(fs, path, fs, 0, ""))
		}
		lo, err := netlink.LinkByName("lo")
		must(err)
		must(netlink.LinkSetUp(lo))
		link, err := netlink.LinkByName("eth0")
		must(err)
		addr, err := netlink.ParseAddr("169.254.17.2/30")
		must(err)
		must(netlink.AddrAdd(link, addr))
		must(netlink.LinkSetUp(link))
		must(netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("169.254.17.1")}))
	}
	http.HandleFunc("/dns", func(w http.ResponseWriter, r *http.Request) {
		ips, err := net.LookupIP("example.test.")
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				fmt.Fprint(w, ip.String())
				return
			}
		}
		http.Error(w, "no IPv4 answer", 502)
	})
	var count atomic.Int64
	http.HandleFunc("/mtu", func(w http.ResponseWriter, r *http.Request) {
		link, err := net.InterfaceByName("eth0")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, link.MTU)
	})
	http.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, count.Add(1))
	})
	http.HandleFunc("/egress", func(w http.ResponseWriter, r *http.Request) {
		conn, err := net.DialTimeout("tcp", "198.51.100.10:18080", 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer conn.Close()
		must(conn.SetDeadline(time.Now().Add(5 * time.Second)))
		_, err = io.WriteString(conn, "runtime")
		if err == nil {
			err = conn.(*net.TCPConn).CloseWrite()
		}
		var response []byte
		if err == nil {
			response, err = io.ReadAll(conn)
		}
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		_, _ = w.Write(response)
	})
	log.Fatal(http.ListenAndServe(":18081", nil))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
