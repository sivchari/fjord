package cluster

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

const (
	// metalLBNamespace is the namespace metallb's native manifest installs
	// every resource into, hardcoded by that manifest.
	metalLBNamespace = "metallb-system"
	// metalLBAPIVersion is the apiVersion of the IPAddressPool and
	// L2Advertisement custom resources EnsureLoadBalancer creates, matching
	// the CRDs metallb's native manifest (embedded as
	// manifests.MetalLBNative) establishes.
	metalLBAPIVersion = "metallb.io/v1beta1"
	// metalLBPoolName names both the IPAddressPool and the L2Advertisement
	// that references it; fjord only ever configures one pool.
	metalLBPoolName = "fjord"

	// metalLBControllerDeploymentName is metallb's controller Deployment,
	// whose Available condition and webhook caBundle (see
	// waitForMetalLBWebhookReady) gate creating the IPAddressPool and
	// L2Advertisement below: metallb validates both through a webhook that
	// is only reachable once the controller has started and published its
	// serving certificate.
	metalLBControllerDeploymentName = "controller"
	// metalLBWebhookConfigurationName is the ValidatingWebhookConfiguration
	// metallb's controller patches with its own generated caBundle once its
	// webhook server is up.
	metalLBWebhookConfigurationName = "metallb-webhook-configuration"

	// metalLBReadyPollInterval and metalLBReadyTimeout bound
	// waitForMetalLBWebhookReady's polling.
	metalLBReadyPollInterval = 2 * time.Second
	metalLBReadyTimeout      = 3 * time.Minute
)

// EnsureLoadBalancer deploys metallb (in L2 mode) to the cluster and
// configures it to hand out addresses from addressRange (e.g.
// "172.19.255.200-172.19.255.250", a range within the kind docker network's
// own subnet -- see cli's dockerNetworkSubnet/metalLBAddressRange) for
// type: LoadBalancer Services, so they receive an external IP reachable from
// the host. kind alone leaves such Services' external IP <pending> forever.
//
// It applies metallb's native manifest (manifests.MetalLBNative) via
// dynamicClient, waits for metallb's controller to be ready to admit its own
// custom resources, then creates a single IPAddressPool/L2Advertisement pair
// requesting addressRange. It is idempotent.
func EnsureLoadBalancer(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, addressRange string) error {
	mapper, err := newRESTMapper(client.Discovery())
	if err != nil {
		return fmt.Errorf("build rest mapper: %w", err)
	}

	if err := applyManifest(ctx, dynamicClient, mapper, manifests.MetalLBNative); err != nil {
		return fmt.Errorf("apply metallb manifest: %w", err)
	}

	if err := waitForMetalLBWebhookReady(ctx, client); err != nil {
		return err
	}

	// Rebuild the mapper: the manifest just applied above establishes
	// metallb's own CRDs (IPAddressPool, L2Advertisement), which the mapper
	// built before that apply cannot resolve.
	mapper, err = newRESTMapper(client.Discovery())
	if err != nil {
		return fmt.Errorf("build rest mapper: %w", err)
	}

	if err := applyObject(ctx, dynamicClient, mapper, ipAddressPool(addressRange)); err != nil {
		return fmt.Errorf("apply metallb ipaddresspool: %w", err)
	}

	if err := applyObject(ctx, dynamicClient, mapper, l2Advertisement()); err != nil {
		return fmt.Errorf("apply metallb l2advertisement: %w", err)
	}

	return nil
}

// waitForMetalLBWebhookReady blocks until metallb's controller has patched
// its ValidatingWebhookConfiguration with a non-empty caBundle for every
// webhook -- the signal its validating webhook server is actually up.
// Creating an IPAddressPool before then fails admission (the API server
// cannot reach or trust the webhook yet).
func waitForMetalLBWebhookReady(ctx context.Context, client kubernetes.Interface) error {
	err := wait.PollUntilContextTimeout(ctx, metalLBReadyPollInterval, metalLBReadyTimeout, true, func(ctx context.Context) (bool, error) {
		return metalLBWebhookReady(ctx, client)
	})
	if err != nil {
		return fmt.Errorf("wait for metallb webhook ready: %w", err)
	}

	return nil
}

// metalLBWebhookReady reports whether metalLBWebhookConfigurationName exists
// and every one of its webhooks has a non-empty caBundle.
func metalLBWebhookReady(ctx context.Context, client kubernetes.Interface) (bool, error) {
	webhook, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, metalLBWebhookConfigurationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get %q validating webhook configuration: %w", metalLBWebhookConfigurationName, err)
	}

	if len(webhook.Webhooks) == 0 {
		return false, nil
	}

	for i := range webhook.Webhooks {
		if len(webhook.Webhooks[i].ClientConfig.CABundle) == 0 {
			return false, nil
		}
	}

	// A present CABundle does not mean the controller is serving the webhook
	// yet; its endpoint refuses connections until the pod passes readiness.
	// The controller Deployment being Available signals that, so validating
	// an IPAddressPool against it will not hit "connection refused".
	controller, err := client.AppsV1().Deployments(metalLBNamespace).Get(ctx, metalLBControllerDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get %q deployment: %w", metalLBControllerDeploymentName, err)
	}

	return controller.Status.AvailableReplicas >= 1, nil
}

// ipAddressPool builds the IPAddressPool custom resource EnsureLoadBalancer
// applies, requesting addressRange for type: LoadBalancer Services.
func ipAddressPool(addressRange string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": metalLBAPIVersion,
		"kind":       "IPAddressPool",
		"metadata": map[string]any{
			"name":      metalLBPoolName,
			"namespace": metalLBNamespace,
		},
		"spec": map[string]any{
			"addresses": []any{addressRange},
		},
	}}
}

// l2Advertisement builds the L2Advertisement custom resource
// EnsureLoadBalancer applies, advertising metalLBPoolName's addresses over
// L2 (ARP/NDP) so hosts on the same docker network -- including the host
// running kind -- can reach them.
func l2Advertisement() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": metalLBAPIVersion,
		"kind":       "L2Advertisement",
		"metadata": map[string]any{
			"name":      metalLBPoolName,
			"namespace": metalLBNamespace,
		},
		"spec": map[string]any{
			"ipAddressPools": []any{metalLBPoolName},
		},
	}}
}
