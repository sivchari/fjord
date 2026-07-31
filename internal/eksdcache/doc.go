// Package eksdcache downloads EKS-D release assets and caches them on disk,
// keyed by their SHA256 checksum.
//
// Its consumer is internal/componentdir, which downloads the EKS-D
// kubernetes-server tarball to materialise a rask component directory,
// reusing this package's download-and-verify cache rather than fetching the
// tarball twice on repeat runs.
package eksdcache
