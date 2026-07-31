package logger

import (
	"fmt"
	"io"
)

// Level indicates the verbosity of an InfoLogger returned by Logger.V. A
// higher Level is more verbose.
type Level int32

// Logger is fjord's logging interface. It mirrors the shape of kind's
// sigs.k8s.io/kind/pkg/log.Logger so that internal/kind can adapt it for
// calls into kind's APIs, without fjord's own code depending on kind.
type Logger interface {
	// Warn logs a warning message.
	Warn(message string)
	// Warnf logs a formatted warning message.
	Warnf(format string, args ...interface{})
	// Error logs an error message.
	Error(message string)
	// Errorf logs a formatted error message.
	Errorf(format string, args ...interface{})
	// V returns an InfoLogger enabled at the given verbosity level.
	V(level Level) InfoLogger
}

// InfoLogger logs informational messages at a fixed verbosity level.
type InfoLogger interface {
	// Info logs an informational message.
	Info(message string)
	// Infof logs a formatted informational message.
	Infof(format string, args ...interface{})
	// Enabled reports whether this InfoLogger's level is enabled.
	Enabled() bool
}

// stderrLogger writes user facing messages to a writer, gating Info/Infof
// calls by verbosity.
type stderrLogger struct {
	out       io.Writer
	verbosity Level
}

var _ Logger = (*stderrLogger)(nil)

// NewStderr returns a Logger that writes messages to out, enabling
// V(level).Info/Infof for levels less than or equal to verbosity.
func NewStderr(out io.Writer, verbosity Level) Logger {
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

func (l *stderrLogger) V(level Level) InfoLogger {
	return infoLogger{out: l.out, enabled: level <= l.verbosity}
}

// infoLogger implements InfoLogger for a single verbosity level.
type infoLogger struct {
	out     io.Writer
	enabled bool
}

var _ InfoLogger = infoLogger{}

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
