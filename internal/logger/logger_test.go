package logger_test

import (
	"bytes"
	"testing"

	"github.com/sivchari/fjord/internal/logger"
)

func TestStderrLogger_V(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		verbosity   logger.Level
		level       logger.Level
		wantEnabled bool
		wantOutput  string
	}{
		{
			name:        "level below verbosity is enabled",
			verbosity:   1,
			level:       0,
			wantEnabled: true,
			wantOutput:  "hello\nworld 1\n",
		},
		{
			name:        "level equal to verbosity is enabled",
			verbosity:   1,
			level:       1,
			wantEnabled: true,
			wantOutput:  "hello\nworld 1\n",
		},
		{
			name:        "level above verbosity is disabled",
			verbosity:   0,
			level:       1,
			wantEnabled: false,
			wantOutput:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			l := logger.NewStderr(&buf, tt.verbosity)
			info := l.V(tt.level)

			if got := info.Enabled(); got != tt.wantEnabled {
				t.Errorf("Enabled() = %v, want %v", got, tt.wantEnabled)
			}

			info.Info("hello")
			info.Infof("world %d", 1)

			if got := buf.String(); got != tt.wantOutput {
				t.Errorf("output = %q, want %q", got, tt.wantOutput)
			}
		})
	}
}
