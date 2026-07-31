// Package provider defines the substrate-neutral abstraction fjord uses to
// create and manage local clusters.
//
// Provider and its supporting types (CreateOptions, Config, Mount,
// AuthWebhook) describe cluster operations without committing to a specific
// backend. internal/rask implements Provider on top of rask (the only
// backend fjord runs clusters on). Callers should depend on this package's
// Provider interface rather than on internal/rask directly, and tests can
// substitute a fake Provider without needing a real cluster.
package provider
