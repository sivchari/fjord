// Package eksdcache downloads EKS-D release assets and caches them on disk,
// keyed by their SHA256 checksum.
//
// It has two consumers: internal/nodeimage, which downloads the EKS-D
// kubernetes-server tarball to build a kind node image, and
// internal/componentdir, which downloads the same tarball to materialise a
// rask component directory. Both reuse this package's download-and-verify
// cache rather than fetching the tarball twice.
package eksdcache
