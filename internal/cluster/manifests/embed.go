// Package manifests embeds third-party Kubernetes manifests fjord applies
// via a dynamic client rather than reimplementing as typed client-go
// objects, since some span CRDs, RBAC, and workloads too broad to hand-write
// and keep in sync with upstream.
package manifests

import _ "embed"

// ClusterNetworkPolicyCRD is the CustomResourceDefinition for
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy; see
// clusternetworkpolicy-crd.yaml's header comment.
//
//go:embed clusternetworkpolicy-crd.yaml
var ClusterNetworkPolicyCRD []byte
