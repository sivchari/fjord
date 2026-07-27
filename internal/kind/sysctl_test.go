package kind

import "testing"

func TestNeedsSysctlRaise(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		minimum int
		want    bool
		wantErr bool
	}{
		{
			name:    "below minimum",
			current: "128",
			minimum: 512,
			want:    true,
		},
		{
			name:    "at minimum",
			current: "512",
			minimum: 512,
			want:    false,
		},
		{
			name:    "above minimum",
			current: "1048576",
			minimum: 524288,
			want:    false,
		},
		{
			name:    "whitespace around value",
			current: "  128\n",
			minimum: 512,
			want:    true,
		},
		{
			name:    "unparsable value",
			current: "not-a-number",
			minimum: 512,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := needsSysctlRaise(tt.current, tt.minimum)
			if (err != nil) != tt.wantErr {
				t.Fatalf("needsSysctlRaise(%q, %d) error = %v, wantErr %v", tt.current, tt.minimum, err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("needsSysctlRaise(%q, %d) = %v, want %v", tt.current, tt.minimum, got, tt.want)
			}
		})
	}
}
