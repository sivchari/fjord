package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const eksAPITestPrincipalARN = "arn:aws:iam::000000000000:user/alice"

// testCustomGroup is a Kubernetes group name reused across this file's and
// accessentry_test.go's/authenticator_test.go's test fixtures.
const testCustomGroup = "custom-group"

// newEKSAPITestServer returns a Server with the EKS API facade endpoints
// enabled, backed by client (for RBAC materialization), accessEntries, and
// clusterInfo.
func newEKSAPITestServer(client *fake.Clientset, accessEntries AccessEntryStore, clusterInfo ClusterInfoStore) *Server {
	return NewServer(NewInMemoryPrincipalStore(), WithEKSAPI(client, accessEntries, clusterInfo))
}

// jsonRequest issues method against handler at path with an optional JSON
// body (nil for none), returning the recorded response.
func jsonRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}

		reqBody = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reqBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

func TestHandleDescribeCluster_Success(t *testing.T) {
	t.Parallel()

	clusterInfo := NewInMemoryClusterInfoStore()
	if err := clusterInfo.Put(t.Context(), ClusterInfo{Endpoint: "https://127.0.0.1:6443", CertificateAuthorityData: "ZmFrZQ=="}); err != nil {
		t.Fatalf("Put cluster info: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), clusterInfo)

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp describeClusterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Cluster.Name != "my-cluster" || resp.Cluster.Endpoint != "https://127.0.0.1:6443" ||
		resp.Cluster.CertificateAuthority.Data != "ZmFrZQ==" || resp.Cluster.Status != "ACTIVE" {
		t.Errorf("cluster = %+v, want name/endpoint/CA/status to match registered cluster info", resp.Cluster)
	}
}

func TestHandleDescribeCluster_NotFound(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleCreateAccessEntry_Success(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost, "/clusters/my-cluster/access-entries", createAccessEntryRequest{
		PrincipalARN:     eksAPITestPrincipalARN,
		Username:         "alice",
		KubernetesGroups: []string{testCustomGroup},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	entry, err := store.Get(t.Context(), eksAPITestPrincipalARN)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if entry.Username != "alice" || len(entry.KubernetesGroups) != 1 || entry.KubernetesGroups[0] != testCustomGroup {
		t.Errorf("entry = %+v, want username=alice groups=[custom-group]", entry)
	}
}

func TestHandleCreateAccessEntry_MissingPrincipalARN(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost, "/clusters/my-cluster/access-entries", createAccessEntryRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleListAccessEntries(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster/access-entries", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp listAccessEntriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.AccessEntries) != 1 || resp.AccessEntries[0] != eksAPITestPrincipalARN {
		t.Errorf("AccessEntries = %v, want [%s]", resp.AccessEntries, eksAPITestPrincipalARN)
	}
}

func TestHandleDeleteAccessEntry(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodDelete, "/clusters/my-cluster/access-entries?principalArn="+eksAPITestPrincipalARN, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if _, err := store.Get(t.Context(), eksAPITestPrincipalARN); err == nil {
		t.Error("entry still present after delete")
	}
}

func TestHandleDeleteAccessEntry_NotFound(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodDelete, "/clusters/my-cluster/access-entries?principalArn="+eksAPITestPrincipalARN, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleAssociateAccessPolicy_ClusterScope(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(client, store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{PolicyARN: StandardAccessPolicyView, AccessScope: accessScopeRequest{Type: "cluster"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	entry, err := store.Get(t.Context(), eksAPITestPrincipalARN)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(entry.AssociatedPolicies) != 1 || entry.AssociatedPolicies[0].PolicyARN != StandardAccessPolicyView {
		t.Fatalf("AssociatedPolicies = %+v, want a single view policy", entry.AssociatedPolicies)
	}

	name := accessEntryBindingName(eksAPITestPrincipalARN, "view")

	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster role binding %q: %v", name, err)
	}

	if binding.RoleRef.Name != "view" {
		t.Errorf("RoleRef.Name = %q, want %q", binding.RoleRef.Name, "view")
	}

	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != principalGroup(eksAPITestPrincipalARN) {
		t.Errorf("Subjects = %+v, want a single group subject %q", binding.Subjects, principalGroup(eksAPITestPrincipalARN))
	}
}

func TestHandleAssociateAccessPolicy_NamespaceScope(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(client, store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{
			PolicyARN:   StandardAccessPolicyEdit,
			AccessScope: accessScopeRequest{Type: "namespace", Namespaces: []string{"team-a", "team-b"}},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	name := accessEntryBindingName(eksAPITestPrincipalARN, "edit")

	for _, ns := range []string{"team-a", "team-b"} {
		if _, err := client.RbacV1().RoleBindings(ns).Get(t.Context(), name, metav1.GetOptions{}); err != nil {
			t.Errorf("get role binding %q in %q: %v", name, ns, err)
		}
	}
}

func TestHandleAssociateAccessPolicy_UnsupportedPolicy(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/SomeUnknownPolicy"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAssociateAccessPolicy_NamespaceScopeRequiresNamespaces(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{PolicyARN: StandardAccessPolicyEdit, AccessScope: accessScopeRequest{Type: "namespace"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAssociateAccessPolicy_UnknownAccessEntry(t *testing.T) {
	t.Parallel()

	server := newEKSAPITestServer(fake.NewClientset(), NewInMemoryAccessEntryStore(), NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{PolicyARN: StandardAccessPolicyView})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleListAssociatedAccessPolicies(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{
		PrincipalARN:       eksAPITestPrincipalARN,
		AssociatedPolicies: []AssociatedAccessPolicy{{PolicyARN: StandardAccessPolicyView, AccessScopeType: "cluster"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodGet,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp listAssociatedAccessPoliciesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.AssociatedAccessPolicies) != 1 || resp.AssociatedAccessPolicies[0].PolicyARN != StandardAccessPolicyView {
		t.Errorf("AssociatedAccessPolicies = %+v, want a single view policy", resp.AssociatedAccessPolicies)
	}
}

func TestHandleDisassociateAccessPolicy(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(client, store, NewInMemoryClusterInfoStore())
	handler := server.Handler()

	associateRec := jsonRequest(t, handler, http.MethodPost,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN,
		associateAccessPolicyRequest{PolicyARN: StandardAccessPolicyView})
	if associateRec.Code != http.StatusOK {
		t.Fatalf("associate status = %d, want %d; body: %s", associateRec.Code, http.StatusOK, associateRec.Body.String())
	}

	disassociateRec := jsonRequest(t, handler, http.MethodDelete,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN+"&policyArn="+StandardAccessPolicyView, nil)
	if disassociateRec.Code != http.StatusOK {
		t.Fatalf("disassociate status = %d, want %d; body: %s", disassociateRec.Code, http.StatusOK, disassociateRec.Body.String())
	}

	entry, err := store.Get(t.Context(), eksAPITestPrincipalARN)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(entry.AssociatedPolicies) != 0 {
		t.Errorf("AssociatedPolicies = %+v, want empty after disassociate", entry.AssociatedPolicies)
	}

	name := accessEntryBindingName(eksAPITestPrincipalARN, "view")

	_, err = client.RbacV1().ClusterRoleBindings().Get(t.Context(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cluster role binding %q still exists after disassociate: err = %v", name, err)
	}
}

func TestHandleDisassociateAccessPolicy_NotAssociated(t *testing.T) {
	t.Parallel()

	store := NewInMemoryAccessEntryStore()
	if err := store.Put(t.Context(), &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	server := newEKSAPITestServer(fake.NewClientset(), store, NewInMemoryClusterInfoStore())

	rec := jsonRequest(t, server.Handler(), http.MethodDelete,
		"/clusters/my-cluster/access-entries/access-policies?principalArn="+eksAPITestPrincipalARN+"&policyArn="+StandardAccessPolicyView, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_EKSAPIEndpointsDisabledWithoutWithEKSAPI(t *testing.T) {
	t.Parallel()

	server := NewServer(NewInMemoryPrincipalStore())

	// The catch-all "POST /" fake-STS route means an unregistered GET still
	// matches that pattern's subtree, so the mux reports 405 (method not
	// allowed on this path) rather than 404 -- what matters here is that no
	// DescribeCluster handler ran, which 405 confirms just as well as 404
	// would.
	rec := jsonRequest(t, server.Handler(), http.MethodGet, "/clusters/my-cluster", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d (mux should not have registered the EKS API routes)", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestClusterRoleForPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		policyARN string
		want      string
		wantOK    bool
	}{
		{policyARN: StandardAccessPolicyClusterAdmin, want: "cluster-admin", wantOK: true},
		{policyARN: StandardAccessPolicyAdmin, want: "admin", wantOK: true},
		{policyARN: StandardAccessPolicyEdit, want: "edit", wantOK: true},
		{policyARN: StandardAccessPolicyView, want: "view", wantOK: true},
		{policyARN: "arn:aws:eks::aws:cluster-access-policy/Unknown", wantOK: false},
		{policyARN: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.policyARN, func(t *testing.T) {
			t.Parallel()

			got, ok := clusterRoleForPolicy(tt.policyARN)
			if ok != tt.wantOK {
				t.Fatalf("clusterRoleForPolicy(%q) ok = %v, want %v", tt.policyARN, ok, tt.wantOK)
			}

			if ok && got != tt.want {
				t.Errorf("clusterRoleForPolicy(%q) = %q, want %q", tt.policyARN, got, tt.want)
			}
		})
	}
}

func TestAccessEntryBindingName_Deterministic(t *testing.T) {
	t.Parallel()

	a := accessEntryBindingName(eksAPITestPrincipalARN, "view")
	b := accessEntryBindingName(eksAPITestPrincipalARN, "view")

	if a != b {
		t.Errorf("accessEntryBindingName is not deterministic: %q != %q", a, b)
	}

	if other := accessEntryBindingName(eksAPITestPrincipalARN, "edit"); other == a {
		t.Errorf("accessEntryBindingName produced the same name for two different cluster roles: %q", a)
	}
}
