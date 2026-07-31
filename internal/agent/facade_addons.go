package agent

import (
	"fmt"
	"net/http"
	"slices"
)

// dummyAddonVersion is the addonVersion every managed addon reports. fjord
// deploys addons directly from their manifests rather than through EKS's
// addon lifecycle, so it has no real per-addon version to report.
const dummyAddonVersion = "v1.0.0-eksbuild.1"

// managedAddons lists the addon names fjord actually deploys, matching real
// EKS addon names so `aws eks list-addons`/`describe-addon` can query them.
// vpc-cni is intentionally excluded: fjord's rask substrate provides its own
// pod networking, not the VPC CNI plugin.
var managedAddons = []string{"coredns", "kube-proxy", "eks-pod-identity-agent"}

// listAddonsResponse is the response body of GET /clusters/{name}/addons.
type listAddonsResponse struct {
	Addons []string `json:"addons"`
}

// handleListAddons serves GET /clusters/{name}/addons, the EKS API facade
// endpoint `aws eks list-addons` calls, reporting the addons fjord actually
// deploys.
func (s *Server) handleListAddons(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, listAddonsResponse{Addons: managedAddons})
}

// describeAddonResponse is the response body of
// GET /clusters/{name}/addons/{addonName}.
type describeAddonResponse struct {
	Addon describeAddonDetail `json:"addon"`
}

// describeAddonDetail is the addon shape describeAddonResponse embeds.
type describeAddonDetail struct {
	AddonName    string                `json:"addonName"`
	ClusterName  string                `json:"clusterName"`
	Status       string                `json:"status"`
	AddonVersion string                `json:"addonVersion"`
	Health       describeClusterHealth `json:"health"`
}

// handleDescribeAddon serves GET /clusters/{name}/addons/{addonName}, the
// EKS API facade endpoint `aws eks describe-addon` calls. It reports 404 for
// any addon fjord does not actually deploy.
func (s *Server) handleDescribeAddon(w http.ResponseWriter, r *http.Request) {
	addonName := r.PathValue("addonName")
	if !slices.Contains(managedAddons, addonName) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, fmt.Sprintf("addon %q not found", addonName))

		return
	}

	writeJSONResponse(w, describeAddonResponse{
		Addon: describeAddonDetail{
			AddonName:    addonName,
			ClusterName:  r.PathValue("name"),
			Status:       "ACTIVE",
			AddonVersion: dummyAddonVersion,
			Health:       describeClusterHealth{Issues: []string{}},
		},
	})
}
