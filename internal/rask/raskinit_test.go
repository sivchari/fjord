package rask

import (
	"errors"
	"testing"
	"testing/fstest"
)

func TestResolveRaskInit(t *testing.T) {
	t.Parallel()

	built := fstest.MapFS{raskInitPath: {Data: []byte("ELF...")}}
	notBuilt := fstest.MapFS{"embedded/README.md": {Data: []byte("why this directory exists")}}

	cases := map[string]struct {
		fsys    fstest.MapFS
		goos    string
		want    string
		wantErr error
	}{
		"darwin, built":     {fsys: built, goos: "darwin", want: "ELF..."},
		"linux, built":      {fsys: built, goos: "linux", want: "ELF..."},
		"darwin, not built": {fsys: notBuilt, goos: "darwin", wantErr: errNoRaskInit},
		// Linux runs the control plane as host processes, so a build that
		// never produced rask-init is the normal case there, not a failure.
		"linux, not built": {fsys: notBuilt, goos: "linux"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveRaskInit(tc.fsys, tc.goos)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("resolveRaskInit() error = %v, want %v", err, tc.wantErr)
			}

			if string(got) != tc.want {
				t.Errorf("resolveRaskInit() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveRaskInitReturnsNilNotEmpty pins the distinction rask draws:
// nil means "use your own copy", so returning an empty non-nil slice on
// Linux would send rask down a different path than intended.
func TestResolveRaskInitReturnsNilNotEmpty(t *testing.T) {
	t.Parallel()

	got, err := resolveRaskInit(fstest.MapFS{"embedded/README.md": {}}, "linux")
	if err != nil {
		t.Fatalf("resolveRaskInit() error = %v", err)
	}

	if got != nil {
		t.Errorf("resolveRaskInit() = %v, want nil", got)
	}
}
