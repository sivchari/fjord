package cluster

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

// EnsureClusterNetworkPolicyCRD registers the networking.k8s.aws/v1alpha1
// ClusterNetworkPolicy CustomResourceDefinition (manifests.ClusterNetworkPolicyCRD),
// so a real EKS cluster's ClusterNetworkPolicy manifests apply unmodified.
// fjord-agent's CNPController (see internal/agent) reconciles the resulting
// custom resources into standard NetworkPolicy objects, since fjord's CNI
// does not itself enforce this CRD. It is idempotent.
func EnsureClusterNetworkPolicyCRD(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface) error {
	mapper, err := newRESTMapper(client.Discovery())
	if err != nil {
		return fmt.Errorf("build rest mapper: %w", err)
	}

	if err := applyManifest(ctx, dynamicClient, mapper, manifests.ClusterNetworkPolicyCRD); err != nil {
		return fmt.Errorf("apply cluster network policy crd: %w", err)
	}

	return nil
}
