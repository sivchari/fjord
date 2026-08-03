package cluster

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

// EnsureNetworkPolicyEnforcer deploys upstream kube-network-policies
// (manifests.KubeNetworkPolicies), the agent that actually enforces
// NetworkPolicy objects. rask's bridge CNI enforces nothing, so without it
// every NetworkPolicy -- including the ones fjord-agent's CNPController
// translates ClusterNetworkPolicies into -- would be silently inert,
// re-opening the pod-IP-direct bypass ClusterNetworkPolicy exists to close.
// Real EKS ships the same capability as the VPC CNI's network policy agent.
// It is idempotent.
func EnsureNetworkPolicyEnforcer(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface) error {
	mapper, err := newRESTMapper(client.Discovery())
	if err != nil {
		return fmt.Errorf("build rest mapper: %w", err)
	}

	if err := applyManifest(ctx, dynamicClient, mapper, manifests.KubeNetworkPolicies); err != nil {
		return fmt.Errorf("apply kube-network-policies: %w", err)
	}

	return nil
}
