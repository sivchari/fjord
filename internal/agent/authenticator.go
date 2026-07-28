package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// eksSchemePrefix is the fixed prefix every EKS bearer token carries;
// `aws eks get-token` (and kubectl's exec credential plugin calling it)
// prepends this to the base64url-encoded presigned GetCallerIdentity URL.
const eksSchemePrefix = "k8s-aws-v1."

// Authenticator serves the Kubernetes API server's authentication token
// webhook (kube-apiserver's --authentication-token-webhook-config-file),
// resolving an EKS-shaped bearer token to a Kubernetes user identity.
//
// Unlike fjord-agent's other components, Authenticator never calls fjord's
// fake STS over HTTP: it is deployed as a control-plane static pod (so
// kube-apiserver can reach it without depending on cluster networking being
// up), and resolves a token's SigV4 access key ID directly against store --
// the same principal registry the fake STS's GetCallerIdentity uses -- and
// its accessEntries, without ever verifying the token's SigV4 signature,
// matching every other fjord component's signature-ignored design (see
// internal/agent/sts.go's resolveAccessKeyID). A principal with no
// registered AccessEntry does not authenticate, matching real EKS's
// requirement that every calling identity have an access entry.
type Authenticator struct {
	store         PrincipalStore
	accessEntries AccessEntryStore
}

// NewAuthenticator returns an Authenticator resolving tokens against store
// and accessEntries.
func NewAuthenticator(store PrincipalStore, accessEntries AccessEntryStore) *Authenticator {
	return &Authenticator{store: store, accessEntries: accessEntries}
}

// Handler returns the http.Handler serving the TokenReview webhook at the
// request root, matching kube-apiserver's webhook client, which posts every
// TokenReview to the single URL its kubeconfig names.
func (a *Authenticator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", a.handleTokenReview)

	return mux
}

// handleTokenReview serves a single TokenReview: it never reports an HTTP
// error for a resolution failure (unknown access key, malformed token, no
// access entry, ...), since that is not how kube-apiserver's webhook client
// distinguishes "this token does not authenticate" from "the webhook itself
// is broken" -- an unauthenticated status, with a 200 response, is what
// tells the union authenticator to try the next authenticator (or ultimately
// reject the request) instead of failing it outright.
func (a *Authenticator) handleTokenReview(w http.ResponseWriter, r *http.Request) {
	var review authenticationv1.TokenReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, "failed to decode token review", http.StatusBadRequest)

		return
	}

	status, err := a.authenticate(r.Context(), review.Spec.Token)
	if err != nil {
		status = &authenticationv1.TokenReviewStatus{Authenticated: false}
	}

	review.Status = *status

	writeJSONResponse(w, review)
}

// authenticate resolves token to a TokenReviewStatus.
func (a *Authenticator) authenticate(ctx context.Context, token string) (*authenticationv1.TokenReviewStatus, error) {
	accessKeyID, err := accessKeyIDFromToken(token)
	if err != nil {
		return nil, err
	}

	principal, err := a.store.GetByAccessKeyID(ctx, accessKeyID)
	if err != nil {
		return nil, fmt.Errorf("resolve principal: %w", err)
	}

	entry, err := a.accessEntries.Get(ctx, principal.ARN)
	if err != nil {
		return nil, fmt.Errorf("resolve access entry: %w", err)
	}

	return &authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User: authenticationv1.UserInfo{
			Username: entry.EffectiveUsername(),
			Groups:   entry.EffectiveGroups(),
			UID:      entry.PrincipalARN,
		},
	}, nil
}

// accessKeyIDFromToken extracts the SigV4 access key ID from an EKS bearer
// token ("k8s-aws-v1.<base64url(presigned GetCallerIdentity URL)>"): the
// same resolution the fake STS applies to a signed request's Authorization
// header (see accessKeyIDFromCredentialScope), except here the SigV4
// credential scope is carried in the presigned URL's own X-Amz-Credential
// query parameter instead of a header.
func accessKeyIDFromToken(token string) (string, error) {
	encoded, ok := strings.CutPrefix(token, eksSchemePrefix)
	if !ok {
		return "", fmt.Errorf("token does not carry the %q prefix", eksSchemePrefix)
	}

	rawURL, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}

	parsed, err := url.Parse(string(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse presigned url: %w", err)
	}

	accessKeyID, ok := accessKeyIDFromCredentialScope(parsed.Query().Get("X-Amz-Credential"))
	if !ok {
		return "", errors.New("presigned url carries no X-Amz-Credential parameter")
	}

	return accessKeyID, nil
}
