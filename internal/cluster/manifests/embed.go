// Package manifests embeds third-party Kubernetes manifests fjord applies
// via a dynamic client rather than reimplementing as typed client-go
// objects, since some (e.g. metallb's native manifest) span CRDs, RBAC, and
// workloads too broad to hand-write and keep in sync with upstream.
package manifests

import _ "embed"

// MetalLBNative is metallb's native install manifest; see
// metallb-native.yaml's header comment for its pinned version and source.
//
//go:embed metallb-native.yaml
var MetalLBNative []byte

// ClusterNetworkPolicyCRD is the CustomResourceDefinition for
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy; see
// clusternetworkpolicy-crd.yaml's header comment.
//
//go:embed clusternetworkpolicy-crd.yaml
var ClusterNetworkPolicyCRD []byte
