// testinit is a minimal static PID-1 binary used inside ateom-ch test VMs.
// It boots, prints a ready line, reaps zombies, and sleeps until killed.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	hostname, _ := os.Hostname()
	fmt.Printf("testinit: VM booted hostname=%s pid=%d\n", hostname, os.Getpid())
	fmt.Println("testinit: ready")

	// PID 1 must reap adopted children or they become zombies.
	go func() {
		for {
			var ws syscall.WaitStatus
			syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigs
	fmt.Printf("testinit: received %v, shutting down\n", sig)
}
