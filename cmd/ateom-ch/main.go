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
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"

	"github.com/agent-substrate/substrate/cmd/ateom-ch/internal/ch"
	"github.com/agent-substrate/substrate/internal/ateinterceptors"
	ateom "github.com/agent-substrate/substrate/internal/ateomlogger"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/contextlogging"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/serverboot"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var (
	podUID      = flag.String("pod-uid", "", "The UID of the current pod")
	showVersion = flag.Bool("version", false, "Print version and exit.")
)

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Println("ateom-ch dev")
		return
	}
	if err := do(context.Background()); err != nil {
		slog.Error("Fatal error", slog.Any("err", err))
		os.Exit(1)
	}
}

func do(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	syncedWriter := ateom.NewSyncedWriter(os.Stdout)
	logger := slog.New(contextlogging.NewHandler(slog.NewJSONHandler(syncedWriter, nil)))
	slog.SetDefault(logger)

	slog.InfoContext(ctx, "ateom-ch booting")

	tp, err := serverboot.InitTracing(ctx, serverboot.TracingOptions{
		ServiceName: "ateom-ch",
		Sampler:     sdktrace.ParentBased(sdktrace.NeverSample()),
		NoExporter:  true,
	})
	if err != nil {
		serverboot.Fatal(ctx, "Failed to initialize tracing", err)
	}
	defer serverboot.ShutdownProvider("TracerProvider", tp.Shutdown)

	// Reuse ateompath socket path so atelet can find us the same way it finds
	// ateom-gvisor — no atelet changes required for phase 2.
	sockPath := ateompath.AteomSocketPath(*podUID)
	if err := os.MkdirAll(ateompath.AteomPath(*podUID), 0o700); err != nil {
		return fmt.Errorf("mkdir ateom dir: %w", err)
	}
	_ = os.Remove(sockPath)

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on unix socket: %w", err)
	}

	actorLogger := ateom.NewActorLogger(syncedWriter, false)
	svc := &AteomService{
		podUID:      *podUID,
		actorLogger: actorLogger,
	}

	svr := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(ateinterceptors.ServerUnaryInterceptor),
	)
	ateompb.RegisterAteomServer(svr, svc)
	reflection.Register(svr)

	svc.warmUp(ctx)

	slog.InfoContext(ctx, "ateom-ch listening", slog.String("sock", sockPath))
	if err := svr.Serve(lis); err != nil {
		return fmt.Errorf("grpc serve: %w", err)
	}
	return nil
}

// runningActor holds handles to the processes and resources of the live VM.
type runningActor struct {
	chCmd      *exec.Cmd
	vfCmd      *exec.Cmd
	chClient   *ch.Client
	tapActorID string
	vfsockPath string
}

// AteomService implements ateompb.AteomServer using Cloud Hypervisor.
type AteomService struct {
	ateompb.UnimplementedAteomServer

	lock        sync.Mutex
	podUID      string
	actorLogger *ateom.ActorLogger

	// prewarm holds a pre-started CH process ready for VmCreate or VmRestore.
	// Nil until warmUp completes or doPrewarm fires. Consumed by acquireVM.
	prewarm *prewarmCHState

	// running is non-nil when a VM is currently executing.
	running *runningActor
}

var _ ateompb.AteomServer = (*AteomService)(nil)
