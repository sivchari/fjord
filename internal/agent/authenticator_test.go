package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// eksBearerToken builds an EKS-shaped bearer token
// ("k8s-aws-v1.<base64url(presigned URL)>") carrying accessKeyID in its
// presigned URL's X-Amz-Credential parameter, matching what `aws eks
// get-token` produces.
func eksBearerToken(accessKeyID string) string {
	presignedURL := "https://sts.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15" +
		"&X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=" + accessKeyID + "%2F20260101%2Fus-east-1%2Fsts%2Faws4_request" +
		"&X-Amz-Date=20260101T000000Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host&X-Amz-Signature=deadbeef"

	return eksSchemePrefix + base64.RawURLEncoding.EncodeToString([]byte(presignedURL))
}

func postTokenReview(t *testing.T, handler http.Handler, token string) *authenticationv1.TokenReview {
	t.Helper()

	body, err := json.Marshal(authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	})
	if err != nil {
		t.Fatalf("marshal token review: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var review authenticationv1.TokenReview
	if err := json.Unmarshal(rec.Body.Bytes(), &review); err != nil {
		t.Fatalf("unmarshal token review response: %v", err)
	}

	return &review
}

func TestAuthenticator_Authenticate_Success(t *testing.T) {
	t.Parallel()

	principals := NewInMemoryPrincipalStore()
	if err := principals.Put(t.Context(), Principal{AccessKeyID: "AKIATEST", ARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put principal: %v", err)
	}

	accessEntries := NewInMemoryAccessEntryStore()
	if err := accessEntries.Put(t.Context(), &AccessEntry{
		PrincipalARN:     eksAPITestPrincipalARN,
		Username:         "alice",
		KubernetesGroups: []string{testCustomGroup},
		AssociatedPolicies: []AssociatedAccessPolicy{
			{PolicyARN: StandardAccessPolicyView, AccessScopeType: "cluster"},
		},
	}); err != nil {
		t.Fatalf("Put access entry: %v", err)
	}

	authenticator := NewAuthenticator(principals, accessEntries)

	review := postTokenReview(t, authenticator.Handler(), eksBearerToken("AKIATEST"))

	if !review.Status.Authenticated {
		t.Fatalf("Authenticated = false, want true")
	}

	if review.Status.User.Username != "alice" {
		t.Errorf("Username = %q, want %q", review.Status.User.Username, "alice")
	}

	wantGroups := []string{testCustomGroup, principalGroup(eksAPITestPrincipalARN)}
	if len(review.Status.User.Groups) != len(wantGroups) {
		t.Fatalf("Groups = %v, want %v", review.Status.User.Groups, wantGroups)
	}

	for i, g := range wantGroups {
		if review.Status.User.Groups[i] != g {
			t.Errorf("Groups[%d] = %q, want %q", i, review.Status.User.Groups[i], g)
		}
	}

	if review.Status.User.UID != eksAPITestPrincipalARN {
		t.Errorf("UID = %q, want %q", review.Status.User.UID, eksAPITestPrincipalARN)
	}
}

func TestAuthenticator_Authenticate_UnknownAccessKey(t *testing.T) {
	t.Parallel()

	authenticator := NewAuthenticator(NewInMemoryPrincipalStore(), NewInMemoryAccessEntryStore())

	review := postTokenReview(t, authenticator.Handler(), eksBearerToken("AKIAGHOST"))

	if review.Status.Authenticated {
		t.Errorf("Authenticated = true, want false for an unregistered access key")
	}
}

func TestAuthenticator_Authenticate_PrincipalWithoutAccessEntry(t *testing.T) {
	t.Parallel()

	principals := NewInMemoryPrincipalStore()
	if err := principals.Put(t.Context(), Principal{AccessKeyID: "AKIATEST", ARN: eksAPITestPrincipalARN}); err != nil {
		t.Fatalf("Put principal: %v", err)
	}

	authenticator := NewAuthenticator(principals, NewInMemoryAccessEntryStore())

	review := postTokenReview(t, authenticator.Handler(), eksBearerToken("AKIATEST"))

	if review.Status.Authenticated {
		t.Errorf("Authenticated = true, want false for a principal with no registered access entry")
	}
}

func TestAuthenticator_Authenticate_MalformedToken(t *testing.T) {
	t.Parallel()

	authenticator := NewAuthenticator(NewInMemoryPrincipalStore(), NewInMemoryAccessEntryStore())

	tests := []struct {
		name  string
		token string
	}{
		{name: "missing prefix", token: "not-a-token"},
		{name: "invalid base64", token: eksSchemePrefix + "!!!not-base64!!!"},
		{name: "no credential parameter", token: eksSchemePrefix + base64.RawURLEncoding.EncodeToString([]byte("https://sts.amazonaws.com/?Action=GetCallerIdentity"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			review := postTokenReview(t, authenticator.Handler(), tt.token)
			if review.Status.Authenticated {
				t.Errorf("Authenticated = true, want false for token %q", tt.token)
			}
		})
	}
}

func TestAccessKeyIDFromToken(t *testing.T) {
	t.Parallel()

	token := eksBearerToken("AKIATEST")

	got, err := accessKeyIDFromToken(token)
	if err != nil {
		t.Fatalf("accessKeyIDFromToken: %v", err)
	}

	if got != "AKIATEST" {
		t.Errorf("accessKeyIDFromToken() = %q, want %q", got, "AKIATEST")
	}
}
