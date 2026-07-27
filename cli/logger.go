package cli

import (
	"fmt"
	"io"

	"sigs.k8s.io/kind/pkg/log"
)

// stderrLogger is a minimal sigs.k8s.io/kind/pkg/log.Logger implementation
// writing user facing messages to a writer. kind's own cli logger lives in an
// internal package and cannot be imported.
type stderrLogger struct {
	out       io.Writer
	verbosity log.Level
}

var _ log.Logger = (*stderrLogger)(nil)

func newLogger(out io.Writer, verbosity log.Level) *stderrLogger {
	return &stderrLogger{out: out, verbosity: verbosity}
}

func (l *stderrLogger) Warn(message string) {
	_, _ = fmt.Fprintln(l.out, message)
}

func (l *stderrLogger) Warnf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *stderrLogger) Error(message string) {
	_, _ = fmt.Fprintln(l.out, message)
}

func (l *stderrLogger) Errorf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *stderrLogger) V(level log.Level) log.InfoLogger {
	return infoLogger{out: l.out, enabled: level <= l.verbosity}
}

// infoLogger implements log.InfoLogger for a single verbosity level.
type infoLogger struct {
	out     io.Writer
	enabled bool
}

var _ log.InfoLogger = infoLogger{}

func (l infoLogger) Info(message string) {
	if l.enabled {
		_, _ = fmt.Fprintln(l.out, message)
	}
}

func (l infoLogger) Infof(format string, args ...interface{}) {
	if l.enabled {
		_, _ = fmt.Fprintf(l.out, format+"\n", args...)
	}
}

func (l infoLogger) Enabled() bool {
	return l.enabled
}
