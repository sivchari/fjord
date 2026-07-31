// Package provider defines the substrate-neutral abstraction fjord uses to
// create and manage local clusters.
//
// Provider and its supporting types (CreateOptions, Config, Mount,
// AuthWebhook) describe cluster operations without committing to a specific
// backend. internal/kind implements Provider on top of sigs.k8s.io/kind; a
// future implementation may shell out to the rask CLI instead. Callers
// outside internal/kind should depend on this package's Provider interface
// rather than on internal/kind directly.
package provider
