package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// parseServiceAccountUsernameTestCase is a single
// TestParseServiceAccountUsername case.
type parseServiceAccountUsernameTestCase struct {
	name               string
	username           string
	wantNamespace      string
	wantServiceAccount string
	wantOK             bool
}

// parseServiceAccountUsernameTestCases holds
// TestParseServiceAccountUsername's cases, factored out as a package-level
// value so the test function itself stays short.
var parseServiceAccountUsernameTestCases = []parseServiceAccountUsernameTestCase{
	{
		name:               "valid service account username",
		username:           "system:serviceaccount:default:app-sa",
		wantNamespace:      "default",
		wantServiceAccount: "app-sa",
		wantOK:             true,
	},
	{name: "missing prefix", username: "system:node:ip-10-0-0-1", wantOK: false},
	{name: "missing service account name", username: "system:serviceaccount:default:", wantOK: false},
	{name: "missing namespace", username: "system:serviceaccount::app-sa", wantOK: false},
	{name: "no colon separator after namespace", username: "system:serviceaccount:default", wantOK: false},
	{name: "empty username", username: "", wantOK: false},
}

func TestParseServiceAccountUsername(t *testing.T) {
	t.Parallel()

	for _, tt := range parseServiceAccountUsernameTestCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			namespace, serviceAccount, ok := parseServiceAccountUsername(tt.username)
			if ok != tt.wantOK {
				t.Fatalf("parseServiceAccountUsername(%q) ok = %v, want %v", tt.username, ok, tt.wantOK)
			}

			if !ok {
				return
			}

			if namespace != tt.wantNamespace || serviceAccount != tt.wantServiceAccount {
				t.Errorf("parseServiceAccountUsername(%q) = (%q, %q), want (%q, %q)",
					tt.username, namespace, serviceAccount, tt.wantNamespace, tt.wantServiceAccount)
			}
		})
	}
}

// reactToTokenReview installs a reactor on client that responds to every
// TokenReview Create with a review whose Status matches result, ignoring the
// submitted Spec.
func reactToTokenReview(client *fake.Clientset, result *authenticationv1.TokenReviewStatus) {
	client.PrependReactor("create", "tokenreviews", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{Status: *result}, nil
	})
}

// newPodIdentityTestServer returns a Server with EKS Pod Identity endpoints
// enabled, backed by client (for TokenReview) and store (for association
// lookups).
func newPodIdentityTestServer(client *fake.Clientset, store PodIdentityStore) *Server {
	return NewServer(NewInMemoryPrincipalStore(), WithPodIdentity(client, store))
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

func TestHandleAssumeRoleForPodIdentity_Success(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	reactToTokenReview(client, &authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "system:serviceaccount:default:app-sa"},
	})

	store := NewInMemoryPodIdentityStore()
	if err := store.Put(context.Background(), PodIdentityAssociation{
		Namespace:      "default",
		ServiceAccount: "app-sa",
		RoleARN:        "arn:aws:iam::000000000000:role/app-role",
		AssociationID:  "a-1234567890123456",
	}); err != nil {
		t.Fatalf("Put association: %v", err)
	}

	server := newPodIdentityTestServer(client, store)

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "any-token"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp assumeRoleForPodIdentityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, rec.Body.String())
	}

	assertAssumeRoleForPodIdentityResponse(t, &resp)

	// The issued credentials must resolve, via fjord-agent's fake STS
	// (backed by the same PrincipalStore), to the assumed-role ARN -- the
	// same cross-process registration IMDS relies on.
	principal, err := server.store.GetByAccessKeyID(context.Background(), resp.Credentials.AccessKeyID)
	if err != nil {
		t.Fatalf("GetByAccessKeyID: %v", err)
	}

	if principal.ARN != resp.AssumedRoleUser.ARN {
		t.Errorf("registered principal ARN = %q, want %q", principal.ARN, resp.AssumedRoleUser.ARN)
	}
}

// assertAssumeRoleForPodIdentityResponse verifies resp carries the shape
// TestHandleAssumeRoleForPodIdentity_Success's fixture association
// (namespace "default", ServiceAccount "app-sa", role "app-role",
// association ID "a-1234567890123456") should produce.
func assertAssumeRoleForPodIdentityResponse(t *testing.T, resp *assumeRoleForPodIdentityResponse) {
	t.Helper()

	if resp.Audience != podIdentityAudience {
		t.Errorf("audience = %q, want %q", resp.Audience, podIdentityAudience)
	}

	wantARNPrefix := "arn:aws:sts::000000000000:assumed-role/app-role/"
	if !bytes.HasPrefix([]byte(resp.AssumedRoleUser.ARN), []byte(wantARNPrefix)) {
		t.Errorf("assumedRoleUser.arn = %q, want prefix %q", resp.AssumedRoleUser.ARN, wantARNPrefix)
	}

	if resp.Credentials.AccessKeyID == "" || resp.Credentials.SecretAccessKey == "" || resp.Credentials.SessionToken == "" {
		t.Errorf("credentials incomplete: %+v", resp.Credentials)
	}

	if resp.Credentials.Expiration <= 0 {
		t.Errorf("credentials.expiration = %d, want a positive epoch seconds value", resp.Credentials.Expiration)
	}

	wantAssociationID := "a-1234567890123456"
	if resp.PodIdentityAssociation.AssociationID != wantAssociationID {
		t.Errorf("podIdentityAssociation.associationId = %q, want %q", resp.PodIdentityAssociation.AssociationID, wantAssociationID)
	}

	wantAssociationARN := "arn:aws:eks:us-east-1:000000000000:podidentityassociation/my-cluster/" + wantAssociationID
	if resp.PodIdentityAssociation.AssociationARN != wantAssociationARN {
		t.Errorf("podIdentityAssociation.associationArn = %q, want %q", resp.PodIdentityAssociation.AssociationARN, wantAssociationARN)
	}

	if resp.Subject.Namespace != "default" || resp.Subject.ServiceAccount != "app-sa" {
		t.Errorf("subject = %+v, want {default app-sa}", resp.Subject)
	}
}

func TestHandleAssumeRoleForPodIdentity_MissingToken(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	if got := rec.Header().Get("X-Amzn-ErrorType"); got != eksAuthErrorInvalidParameter {
		t.Errorf("X-Amzn-ErrorType = %q, want %q", got, eksAuthErrorInvalidParameter)
	}
}

func TestHandleAssumeRoleForPodIdentity_Unauthenticated(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	reactToTokenReview(client, &authenticationv1.TokenReviewStatus{Authenticated: false})

	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "bad-token"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	if got := rec.Header().Get("X-Amzn-ErrorType"); got != eksAuthErrorInvalidToken {
		t.Errorf("X-Amzn-ErrorType = %q, want %q", got, eksAuthErrorInvalidToken)
	}
}

func TestHandleAssumeRoleForPodIdentity_NonServiceAccountToken(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	reactToTokenReview(client, &authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "system:node:ip-10-0-0-1"},
	})

	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "node-token"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleAssumeRoleForPodIdentity_NoAssociation(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	reactToTokenReview(client, &authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "system:serviceaccount:default:app-sa"},
	})

	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "any-token"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	if got := rec.Header().Get("X-Amzn-ErrorType"); got != eksAuthErrorResourceNotFound {
		t.Errorf("X-Amzn-ErrorType = %q, want %q", got, eksAuthErrorResourceNotFound)
	}
}

// TestServer_PodIdentityEndpointsDisabledWithoutWithPodIdentity verifies
// that a Server built without WithPodIdentity never mounts the EKS Pod
// Identity endpoints, so existing STS-only deployments are unaffected.
func TestServer_PodIdentityEndpointsDisabledWithoutWithPodIdentity(t *testing.T) {
	t.Parallel()

	server := NewServer(NewInMemoryPrincipalStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "any-token"})

	// mux's "POST /" fake-STS pattern is a subtree match covering every
	// path, so an unmounted Pod Identity route falls through to dispatchSTS
	// rather than 404ing; assert on its XML STS-error shape instead, which
	// proves handleAssumeRoleForPodIdentity was never reached.
	if got := rec.Header().Get("Content-Type"); got != "text/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the fake STS's text/xml (pod identity route should not be mounted)", got)
	}
}

// TestNewPodIdentitySessionName verifies the session name embeds
// serviceAccount and stays reasonably unique across calls.
func TestNewPodIdentitySessionName(t *testing.T) {
	t.Parallel()

	first, err := newPodIdentitySessionName("app-sa")
	if err != nil {
		t.Fatalf("newPodIdentitySessionName() error = %v", err)
	}

	second, err := newPodIdentitySessionName("app-sa")
	if err != nil {
		t.Fatalf("newPodIdentitySessionName() error = %v", err)
	}

	if first == second {
		t.Errorf("newPodIdentitySessionName() produced the same value twice: %q", first)
	}

	wantPrefix := "eks-app-sa-"
	if !bytes.HasPrefix([]byte(first), []byte(wantPrefix)) {
		t.Errorf("newPodIdentitySessionName() = %q, want prefix %q", first, wantPrefix)
	}
}

// TestHandleAssumeRoleForPodIdentity_TokenReviewError verifies a TokenReview
// call failure (e.g. the API server rejecting the request) surfaces as 401,
// not a 5xx that would let a caller distinguish "invalid token" from
// "fjord-agent is misbehaving".
func TestHandleAssumeRoleForPodIdentity_TokenReviewError(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated api server error")
	})

	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "any-token"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
