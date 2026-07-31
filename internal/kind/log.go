package kind

import (
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/logger"
)

// logAdapter adapts fjord's logger.Logger to kind's log.Logger, so that
// fjord's own logging vocabulary can be passed into kind's APIs
// (kindcluster.ProviderWithLogger, nodeimage.WithLogger). This is the only
// place in fjord that mentions sigs.k8s.io/kind/pkg/log.
type logAdapter struct {
	inner logger.Logger
}

var _ log.Logger = logAdapter{}

func (l logAdapter) Warn(message string) {
	l.inner.Warn(message)
}

func (l logAdapter) Warnf(format string, args ...interface{}) {
	l.inner.Warnf(format, args...)
}

func (l logAdapter) Error(message string) {
	l.inner.Error(message)
}

func (l logAdapter) Errorf(format string, args ...interface{}) {
	l.inner.Errorf(format, args...)
}

func (l logAdapter) V(level log.Level) log.InfoLogger {
	return infoLoggerAdapter{inner: l.inner.V(logger.Level(level))}
}

// infoLoggerAdapter adapts fjord's logger.InfoLogger to kind's
// log.InfoLogger.
type infoLoggerAdapter struct {
	inner logger.InfoLogger
}

var _ log.InfoLogger = infoLoggerAdapter{}

func (l infoLoggerAdapter) Info(message string) {
	l.inner.Info(message)
}

func (l infoLoggerAdapter) Infof(format string, args ...interface{}) {
	l.inner.Infof(format, args...)
}

func (l infoLoggerAdapter) Enabled() bool {
	return l.inner.Enabled()
}
