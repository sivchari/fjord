package agent

import (
	"encoding/json"
	"net/http"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestHandleListAddons(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster/addons", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp listAddonsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	want := []string{"coredns", "kube-proxy", "eks-pod-identity-agent"}
	if len(resp.Addons) != len(want) {
		t.Fatalf("Addons = %v, want %v", resp.Addons, want)
	}

	for i, addon := range want {
		if resp.Addons[i] != addon {
			t.Errorf("Addons[%d] = %q, want %q", i, resp.Addons[i], addon)
		}
	}
}

func TestHandleDescribeAddon_Success(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster/addons/coredns", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp describeAddonResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Addon.AddonName != "coredns" || resp.Addon.ClusterName != eksAPITestClusterName || resp.Addon.Status != "ACTIVE" {
		t.Errorf("Addon = %+v, want addonName=coredns clusterName=%s status=ACTIVE", resp.Addon, eksAPITestClusterName)
	}

	if resp.Addon.AddonVersion == "" {
		t.Error("AddonVersion is empty")
	}

	if resp.Addon.Health.Issues == nil {
		t.Error("Health.Issues is nil, want a non-nil (possibly empty) slice")
	}
}

func TestHandleDescribeAddon_NotFound(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster/addons/vpc-cni", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
