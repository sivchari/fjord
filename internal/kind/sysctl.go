package kind

import (
	"fmt"
	"sort"
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

// inotifySysctlConfPath is where raiseInotifyLimits persists the limits on
// each node. The node's shared VM kernel (Docker Desktop) resets sysctls on
// VM restart; systemd-sysctl re-applies this file when the node container
// boots, so the limits survive.
const inotifySysctlConfPath = "/etc/sysctl.d/99-fjord-inotify.conf"

// raiseInotifyLimits raises the inotify sysctls on every node of the cluster
// to at least the recommended minimum and persists them under
// /etc/sysctl.d so node restarts re-apply them. Values already at or above
// the minimum are left untouched for the running kernel.
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

		conf := inotifySysctlConf()

		cmd := node.Command("cp", "/dev/stdin", inotifySysctlConfPath)
		cmd.SetStdin(strings.NewReader(conf))

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("persist %s on node %s: %w", inotifySysctlConfPath, node.String(), err)
		}
	}

	return nil
}

// inotifySysctlConf renders the sysctl.d file body persisting
// minInotifySysctls, with keys in deterministic order.
func inotifySysctlConf() string {
	keys := make([]string, 0, len(minInotifySysctls))
	for key := range minInotifySysctls {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	var b strings.Builder

	b.WriteString("# Managed by fjord: inotify limits kube-proxy and kubelet need.\n")

	for _, key := range keys {
		fmt.Fprintf(&b, "%s = %d\n", key, minInotifySysctls[key])
	}

	return b.String()
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
