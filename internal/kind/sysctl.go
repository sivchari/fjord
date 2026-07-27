package kind

import (
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/kind/pkg/exec"
)

// minInotifySysctls are the inotify limits cluster nodes need. kube-proxy
// (EKS-D builds in particular) exhausts Docker Desktop's default
// fs.inotify.max_user_instances=128 and crash-loops with "too many open
// files", which leaves CoreDNS unable to reach the API server. These match
// the values the kind documentation recommends.
var minInotifySysctls = map[string]int{
	"fs.inotify.max_user_watches":   524288,
	"fs.inotify.max_user_instances": 512,
}

// raiseInotifyLimits raises the inotify sysctls on every node of the cluster
// to at least the recommended minimum. Values already at or above the minimum
// are left untouched.
func (p *provider) raiseInotifyLimits(name string) error {
	nodes, err := p.inner.ListNodes(name)
	if err != nil {
		return fmt.Errorf("list nodes for cluster %q: %w", name, err)
	}

	for _, node := range nodes {
		for key, minimum := range minInotifySysctls {
			lines, err := exec.OutputLines(node.Command("sysctl", "-n", key))
			if err != nil {
				return fmt.Errorf("read %s on node %s: %w", key, node.String(), err)
			}

			raise, err := needsSysctlRaise(strings.Join(lines, ""), minimum)
			if err != nil {
				return fmt.Errorf("parse %s on node %s: %w", key, node.String(), err)
			}

			if !raise {
				continue
			}

			set := fmt.Sprintf("%s=%d", key, minimum)
			if err := node.Command("sysctl", "-w", set).Run(); err != nil {
				return fmt.Errorf("set %s on node %s: %w", set, node.String(), err)
			}
		}
	}

	return nil
}

// needsSysctlRaise reports whether a sysctl currently at value current must
// be raised to reach minimum.
func needsSysctlRaise(current string, minimum int) (bool, error) {
	value, err := strconv.Atoi(strings.TrimSpace(current))
	if err != nil {
		return false, fmt.Errorf("unexpected sysctl value %q: %w", current, err)
	}

	return value < minimum, nil
}
