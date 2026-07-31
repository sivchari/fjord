// Package kind isolates fjord's dependency on sigs.k8s.io/kind.
//
// kind has not reached v1.0 and its public API is not guaranteed stable
// across releases. Rather than depending on kind directly throughout fjord,
// every other package talks to the substrate-neutral internal/provider
// surface, which this package implements. When bumping the kind version
// introduces a breaking change, this package is the only place that needs
// to change.
package kind
