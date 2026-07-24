package eksd

// Asset is a downloadable EKS-D artifact identified by its URI and SHA256
// checksum.
type Asset struct {
	URI    string
	SHA256 string
}

// Release holds the EKS-D release information needed to build a node image
// for a given EKS Kubernetes minor version.
type Release struct {
	// EKSVersion is the EKS Kubernetes minor version, e.g. "1.33".
	EKSVersion string
	// Channel is the EKS-D release channel, e.g. "1-33".
	Channel string
	// Number is the EKS-D patch number within Channel.
	Number int
	// KubeVersion is the upstream Kubernetes version, e.g. "v1.33.5".
	KubeVersion string
	// ServerTarball maps arch ("amd64", "arm64") to the
	// kubernetes-server-linux-<arch>.tar.gz asset.
	ServerTarball map[string]Asset
	// CoreDNSImage is the full EKS-D coredns container image URI.
	CoreDNSImage string
	// KubeProxyImage is the full EKS-D kube-proxy container image URI.
	KubeProxyImage string
}
