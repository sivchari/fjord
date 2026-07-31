package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/agent"
)

// startLoadBalancerController starts fjord's LoadBalancer controller (see
// internal/agent.LoadBalancerController) in the background, deriving its
// own signal-aware context so it shuts down gracefully alongside serveAPI's
// HTTP servers rather than being killed mid-reconcile when the process
// exits. The returned stop function cancels the controller and blocks
// until it has exited; callers must call it (e.g. via defer) before
// returning, to avoid leaking its goroutine.
func startLoadBalancerController(cmd *cobra.Command, clientset kubernetes.Interface) (stop func()) {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	logf := func(format string, args ...any) { cmd.PrintErrln(fmt.Sprintf(format, args...)) }
	controller := agent.NewLoadBalancerController(clientset, logf)

	go func() {
		if err := controller.Run(ctx); err != nil {
			logf("load balancer controller: %v", err)
		}
	}()

	return stop
}
