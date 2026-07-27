// Package nodeimage builds kind node images from EKS-D server tarballs.
//
// The pipeline downloads the EKS-D kubernetes-server tarball for a given
// EKS Kubernetes minor version and architecture, rewrites the EKS-D
// container image references embedded in it (public.ecr.aws/eks-distro/...)
// to their upstream registry.k8s.io equivalents, and hands the result to
// internal/kind.BuildNodeImage.
package nodeimage
