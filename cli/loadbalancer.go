package cli

import (
	"fmt"
	"net"
	"strings"
)

// ipv4Subnet returns the single IPv4 CIDR among the space-separated subnets
// docker reports for a network (which also includes an IPv6 subnet).
func ipv4Subnet(subnets string) (string, error) {
	for _, field := range strings.Fields(subnets) {
		ip, _, err := net.ParseCIDR(field)
		if err != nil {
			continue
		}

		if ip.To4() != nil {
			return field, nil
		}
	}

	return "", fmt.Errorf("no IPv4 subnet in %q", subnets)
}

// metalLBAddressRange returns a small address range near the top of cidr for
// metallb's IPAddressPool. It uses the .255.200-.255.250 span of the network,
// well clear of the low addresses kind assigns to node containers. cidr must
// be an IPv4 /16 or wider (kind's default is /16); narrower networks return an
// error rather than a range that overlaps node addresses.
func metalLBAddressRange(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse subnet %q: %w", cidr, err)
	}

	base := ipNet.IP.To4()
	if base == nil {
		return "", fmt.Errorf("subnet %q is not IPv4", cidr)
	}

	ones, _ := ipNet.Mask.Size()
	if ones > 16 {
		return "", fmt.Errorf("subnet %q is narrower than /16; cannot carve a safe address range", cidr)
	}

	first := net.IPv4(base[0], base[1], 255, 200)
	last := net.IPv4(base[0], base[1], 255, 250)

	return fmt.Sprintf("%s-%s", first, last), nil
}
