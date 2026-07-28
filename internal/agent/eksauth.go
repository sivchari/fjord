package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// podIdentityAudience is the projected ServiceAccount token audience the
	// eks-pod-identity-agent (and, mirroring it, fjord's own Injector) uses
	// for EKS Pod Identity, matching the real EKS Auth API's fixed audience.
	podIdentityAudience = "pods.eks.amazonaws.com"

	// DefaultPodIdentityFullURI is the AWS_CONTAINER_CREDENTIALS_FULL_URI
	// value fjord's Injector injects into pods whose ServiceAccount carries
	// a PodIdentityAssociation, matching the link-local address the
	// official eks-pod-identity-agent DaemonSet listens on.
	DefaultPodIdentityFullURI = "http://169.254.170.23/v1/credentials"

	serviceAccountUsernamePrefix = "system:serviceaccount:"

	podIdentitySessionNameBytes = 8

	eksAuthErrorInvalidRequest   = "InvalidRequestException"
	eksAuthErrorInvalidParameter = "InvalidParameterException"
	eksAuthErrorInvalidToken     = "InvalidTokenException"
	eksAuthErrorResourceNotFound = "ResourceNotFoundException"
	eksAuthErrorInternalServer   = "InternalServerException"
)

// assumeRoleForPodIdentityRequest is the request body of
// POST /clusters/{name}/assume-role-for-pod-identity, matching the real EKS
// Auth API's AssumeRoleForPodIdentity operation.
type assumeRoleForPodIdentityRequest struct {
	Token string `json:"token"`
}

// assumeRoleForPodIdentityResponse is the response body of
// POST /clusters/{name}/assume-role-for-pod-identity, matching the shape the
// aws-sdk-go-v2 eksauth client (used by the official eks-pod-identity-agent)
// deserializes.
type assumeRoleForPodIdentityResponse struct {
	AssumedRoleUser        eksAuthAssumedRoleUser        `json:"assumedRoleUser"`
	Audience               string                        `json:"audience"`
	Credentials            eksAuthCredentials            `json:"credentials"`
	PodIdentityAssociation eksAuthPodIdentityAssociation `json:"podIdentityAssociation"`
	Subject                eksAuthSubject                `json:"subject"`
}

// eksAuthAssumedRoleUser is the AssumedRoleUser shape of
// assumeRoleForPodIdentityResponse.
type eksAuthAssumedRoleUser struct {
	ARN          string `json:"arn"`
	AssumeRoleID string `json:"assumeRoleId"`
}

// eksAuthCredentials is the Credentials shape of
// assumeRoleForPodIdentityResponse. Expiration is a Unix epoch seconds
// number, matching the real EKS Auth API's restJson1 timestamp encoding.
type eksAuthCredentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
	Expiration      int64  `json:"expiration"`
}

// eksAuthPodIdentityAssociation is the PodIdentityAssociation shape of
// assumeRoleForPodIdentityResponse.
type eksAuthPodIdentityAssociation struct {
	AssociationARN string `json:"associationArn"`
	AssociationID  string `json:"associationId"`
}

// eksAuthSubject is the Subject shape of assumeRoleForPodIdentityResponse.
type eksAuthSubject struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
}

// handleAssumeRoleForPodIdentity serves
// POST /clusters/{name}/assume-role-for-pod-identity, the endpoint the
// official eks-pod-identity-agent DaemonSet calls (via its --endpoint flag)
// in place of the real EKS Auth API. It authenticates the caller's
// ServiceAccount token via TokenReview, resolves the token's
// (namespace, ServiceAccount) to a registered PodIdentityAssociation, and
// issues temporary credentials for that association's role.
func (s *Server) handleAssumeRoleForPodIdentity(w http.ResponseWriter, r *http.Request) {
	var req assumeRoleForPodIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEKSAuthError(w, http.StatusBadRequest, eksAuthErrorInvalidRequest, "failed to parse request body")

		return
	}

	if req.Token == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksAuthErrorInvalidParameter, "token is required")

		return
	}

	namespace, serviceAccount, err := s.authenticatePodIdentityToken(r.Context(), req.Token)
	if err != nil {
		writeEKSAuthError(w, http.StatusUnauthorized, eksAuthErrorInvalidToken, err.Error())

		return
	}

	association, err := s.podIdentityStore.GetBySA(r.Context(), namespace, serviceAccount)
	if errors.Is(err, ErrPodIdentityAssociationNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksAuthErrorResourceNotFound,
			fmt.Sprintf("no pod identity association for %s/%s", namespace, serviceAccount))

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksAuthErrorInternalServer, err.Error())

		return
	}

	response, err := s.issuePodIdentityCredentials(r.Context(), r.PathValue("name"), association)
	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksAuthErrorInternalServer, err.Error())

		return
	}

	writeJSONResponse(w, response)
}

// authenticatePodIdentityToken validates token via a Kubernetes TokenReview
// (scoped to podIdentityAudience, matching the audience the projected
// ServiceAccount token webhook.amazon-eks-pod-identity-webhook and fjord's
// own Injector inject) and returns the (namespace, ServiceAccount) it
// belongs to.
func (s *Server) authenticatePodIdentityToken(ctx context.Context, token string) (namespace, serviceAccount string, err error) {
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{podIdentityAudience},
		},
	}

	result, err := s.client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("create token review: %w", err)
	}

	if !result.Status.Authenticated {
		return "", "", errors.New("token is not authenticated")
	}

	namespace, serviceAccount, ok := parseServiceAccountUsername(result.Status.User.Username)
	if !ok {
		return "", "", fmt.Errorf("token does not belong to a service account: %q", result.Status.User.Username)
	}

	return namespace, serviceAccount, nil
}

// parseServiceAccountUsername extracts the namespace and ServiceAccount name
// from a Kubernetes authenticated username of the form
// "system:serviceaccount:<namespace>:<name>", the format TokenReview reports
// for a ServiceAccount token.
func parseServiceAccountUsername(username string) (namespace, serviceAccount string, ok bool) {
	rest, ok := strings.CutPrefix(username, serviceAccountUsernamePrefix)
	if !ok {
		return "", "", false
	}

	namespace, serviceAccount, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || serviceAccount == "" {
		return "", "", false
	}

	return namespace, serviceAccount, true
}

// issuePodIdentityCredentials issues temporary credentials for association's
// role, registers them in s.store keyed by the fresh access key ID (the same
// cross-process registration IMDS uses, so a later GetCallerIdentity call
// against fjord-agent's fake STS resolves them), and builds the response
// body clusterName's caller (the eks-pod-identity-agent DaemonSet) expects.
func (s *Server) issuePodIdentityCredentials(ctx context.Context, clusterName string, association *PodIdentityAssociation) (*assumeRoleForPodIdentityResponse, error) {
	credentials, err := newCredentials(time.Now())
	if err != nil {
		return nil, fmt.Errorf("generate pod identity credentials: %w", err)
	}

	sessionName, err := newPodIdentitySessionName(association.ServiceAccount)
	if err != nil {
		return nil, err
	}

	roleUserARN := assumedRoleARN(association.RoleARN, sessionName)

	assumedRoleID, err := newAssumedRoleID(sessionName)
	if err != nil {
		return nil, err
	}

	principal := Principal{
		AccessKeyID: credentials.AccessKeyID,
		ARN:         roleUserARN,
		Name:        association.AssociationID,
	}

	if err := s.store.Put(ctx, principal); err != nil {
		return nil, fmt.Errorf("register pod identity credentials: %w", err)
	}

	expiration, err := time.Parse(time.RFC3339, credentials.Expiration)
	if err != nil {
		return nil, fmt.Errorf("parse credentials expiration: %w", err)
	}

	return &assumeRoleForPodIdentityResponse{
		AssumedRoleUser: eksAuthAssumedRoleUser{
			ARN:          roleUserARN,
			AssumeRoleID: assumedRoleID,
		},
		Audience: podIdentityAudience,
		Credentials: eksAuthCredentials{
			AccessKeyID:     credentials.AccessKeyID,
			SecretAccessKey: credentials.SecretAccessKey,
			SessionToken:    credentials.SessionToken,
			Expiration:      expiration.Unix(),
		},
		PodIdentityAssociation: eksAuthPodIdentityAssociation{
			AssociationARN: PodIdentityAssociationARN(clusterName, association.AssociationID),
			AssociationID:  association.AssociationID,
		},
		Subject: eksAuthSubject{
			Namespace:      association.Namespace,
			ServiceAccount: association.ServiceAccount,
		},
	}, nil
}

// newPodIdentitySessionName returns a random STS role session name for a
// pod identity credential exchange, shaped like the real EKS Auth API's own
// ("eks-<service-account>-<random>").
func newPodIdentitySessionName(serviceAccount string) (string, error) {
	suffix, err := randomHexString(podIdentitySessionNameBytes)
	if err != nil {
		return "", fmt.Errorf("generate pod identity session name: %w", err)
	}

	return fmt.Sprintf("eks-%s-%s", serviceAccount, suffix), nil
}

// writeEKSAuthError writes a restJson1-shaped EKS Auth API error response:
// the error code in the X-Amzn-ErrorType header (which the aws-sdk-go-v2
// eksauth client reads to select the typed error to return) and a JSON body
// carrying message.
func writeEKSAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("X-Amzn-ErrorType", code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// writeJSONResponse writes v as a 200 OK JSON response body. Every caller
// reports success this way; a handler reporting an error uses
// writeEKSAuthError instead.
func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
