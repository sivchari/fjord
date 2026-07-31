// Package rask isolates fjord's dependency on github.com/sivchari/rask.
//
// rask has not reached v1.0 and its public API is not guaranteed stable
// across releases. Rather than depending on rask directly throughout fjord,
// every other package talks to the substrate-neutral internal/provider
// surface, which this package implements. When bumping the rask version
// introduces a breaking change, this package is the only place that needs
// to change.
package rask
