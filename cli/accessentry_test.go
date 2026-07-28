package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sivchari/fjord/internal/agent"
)

// testPrincipalARN is the principal ARN shared by every access entry test
// in this file.
const testPrincipalARN = "arn:aws:iam::000000000000:user/alice"

func TestPostFacadeJSON_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := postFacadeJSON(t.Context(), server.URL, map[string]string{"foo": "bar"}); err != nil {
		t.Fatalf("postFacadeJSON() error = %v", err)
	}

	if gotBody["foo"] != "bar" {
		t.Errorf("gotBody = %v, want {\"foo\":\"bar\"}", gotBody)
	}
}

func TestPostFacadeJSON_Error(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	}))
	defer server.Close()

	err := postFacadeJSON(t.Context(), server.URL, map[string]string{})
	if err == nil {
		t.Fatal("postFacadeJSON() error = nil, want an error")
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to contain %q", err, "boom")
	}
}

func TestCreateAccessEntry(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotReq  createAccessEntryRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	principalARN := testPrincipalARN

	err := createAccessEntry(t.Context(), server.URL+"/clusters/fjord", principalARN, "alice", []string{"viewers"})
	if err != nil {
		t.Fatalf("createAccessEntry() error = %v", err)
	}

	if wantPath := "/clusters/fjord/access-entries"; gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}

	if gotReq.PrincipalARN != principalARN {
		t.Errorf("PrincipalARN = %q, want %q", gotReq.PrincipalARN, principalARN)
	}

	if gotReq.Username != "alice" {
		t.Errorf("Username = %q, want %q", gotReq.Username, "alice")
	}

	if !slices.Equal(gotReq.KubernetesGroups, []string{"viewers"}) {
		t.Errorf("KubernetesGroups = %v, want [viewers]", gotReq.KubernetesGroups)
	}
}

func TestAssociateAccessPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		namespace      string
		wantScopeType  string
		wantNamespaces []string
	}{
		{name: "cluster-wide when namespace is empty", namespace: "", wantScopeType: "cluster"},
		{name: "namespace-scoped when namespace is set", namespace: "team-a", wantScopeType: "namespace", wantNamespaces: []string{"team-a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotQuery, gotReq := runAssociateAccessPolicyAgainstTestServer(t, tt.namespace)

			principalARN := testPrincipalARN
			if got := gotQuery.Get("principalArn"); got != principalARN {
				t.Errorf("principalArn query param = %q, want %q", got, principalARN)
			}

			if gotReq.PolicyARN != agent.StandardAccessPolicyView {
				t.Errorf("PolicyARN = %q, want %q", gotReq.PolicyARN, agent.StandardAccessPolicyView)
			}

			if gotReq.AccessScope.Type != tt.wantScopeType {
				t.Errorf("AccessScope.Type = %q, want %q", gotReq.AccessScope.Type, tt.wantScopeType)
			}

			if !slices.Equal(gotReq.AccessScope.Namespaces, tt.wantNamespaces) {
				t.Errorf("AccessScope.Namespaces = %v, want %v", gotReq.AccessScope.Namespaces, tt.wantNamespaces)
			}
		})
	}
}

// runAssociateAccessPolicyAgainstTestServer calls associateAccessPolicy
// against a test server scoped to namespace, returning the query
// parameters and decoded request body the server observed.
func runAssociateAccessPolicyAgainstTestServer(t *testing.T, namespace string) (url.Values, associateAccessPolicyRequest) {
	t.Helper()

	var (
		gotQuery url.Values
		gotReq   associateAccessPolicyRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()

		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	principalARN := testPrincipalARN

	err := associateAccessPolicy(t.Context(), server.URL+"/clusters/fjord", principalARN, agent.StandardAccessPolicyView, namespace)
	if err != nil {
		t.Fatalf("associateAccessPolicy() error = %v", err)
	}

	return gotQuery, gotReq
}

func TestFacadeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "with message body", body: `{"message":"boom"}`, wantSubstr: "boom"},
		{name: "without a body", body: "", wantSubstr: "facade request failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}

			err := facadeError(resp)
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("facadeError() = %v, want it to contain %q", err, tt.wantSubstr)
			}
		})
	}
}
