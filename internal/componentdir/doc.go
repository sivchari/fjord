// Package componentdir materialises an EKS-D release's kubernetes-server
// tarball as a rask component directory: a flat directory containing
// kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, and
// kube-proxy, the five binaries rask's --component-dir flag requires.
//
// Materialize reuses internal/eksdcache's download-and-verify cache for the
// tarball itself, then extracts and caches the five binaries into a
// sibling, SHA256-keyed directory so repeated cluster creates do not
// re-extract.
package componentdir
