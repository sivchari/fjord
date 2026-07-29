package cli

import "testing"

func TestIPv4Subnet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		subnets string
		want    string
		wantErr bool
	}{
		{
			name:    "picks ipv4 from docker's ipv6+ipv4 output",
			subnets: "fc00:f853:ccd:e793::/64 172.19.0.0/16",
			want:    "172.19.0.0/16",
		},
		{
			name:    "ipv4 only",
			subnets: "172.18.0.0/16",
			want:    "172.18.0.0/16",
		},
		{
			name:    "no ipv4",
			subnets: "fc00:f853:ccd:e793::/64",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ipv4Subnet(tt.subnets)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ipv4Subnet(%q) error = %v, wantErr %v", tt.subnets, err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("ipv4Subnet(%q) = %q, want %q", tt.subnets, got, tt.want)
			}
		})
	}
}

func TestMetalLBAddressRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "kind default /16",
			cidr: "172.19.0.0/16",
			want: "172.19.255.200-172.19.255.250",
		},
		{
			name: "different /16",
			cidr: "172.18.0.0/16",
			want: "172.18.255.200-172.18.255.250",
		},
		{
			name:    "too narrow /24 would overlap node addresses",
			cidr:    "172.19.0.0/24",
			wantErr: true,
		},
		{
			name:    "ipv6 rejected",
			cidr:    "fc00::/64",
			wantErr: true,
		},
		{
			name:    "invalid",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := metalLBAddressRange(tt.cidr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("metalLBAddressRange(%q) error = %v, wantErr %v", tt.cidr, err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("metalLBAddressRange(%q) = %q, want %q", tt.cidr, got, tt.want)
			}
		})
	}
}
