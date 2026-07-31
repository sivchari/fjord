package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/agent"
)

// startTargetGroupBindingController starts fjord's TargetGroupBinding
// controller (see internal/agent.TargetGroupBindingController) in the
// background, deriving its own signal-aware context so it shuts down
// gracefully alongside serveAPI's HTTP servers rather than being killed
// mid-reconcile when the process exits. The returned stop function cancels
// the controller and blocks until it has exited; callers must call it
// (e.g. via defer) before returning, to avoid leaking its goroutine.
func startTargetGroupBindingController(parent context.Context, clientset kubernetes.Interface, dynamicClient dynamic.Interface) (stop func()) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	controller := agent.NewTargetGroupBindingController(clientset, dynamicClient, nil)

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := controller.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "target group binding controller: %v\n", err)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
