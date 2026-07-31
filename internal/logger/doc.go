// Package logger defines fjord's own logging vocabulary.
//
// fjord is migrating off kind, so kind's sigs.k8s.io/kind/pkg/log package
// must not be fjord's logging interface. cli and every other package that
// needs to log talk to Logger defined here; internal/kind is the only
// place that adapts it to kind's log.Logger for calls into kind's APIs.
package logger
