package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	stsXMLNS = "https://sts.amazonaws.com/doc/2011-06-15/"

	actionAssumeRoleWithWebIdentity = "AssumeRoleWithWebIdentity"
	actionGetCallerIdentity         = "GetCallerIdentity"

	stsErrorInvalidAction    = "InvalidAction"
	stsErrorMissingParameter = "MissingParameter"
	stsErrorInternalFailure  = "InternalFailure"

	sessionCredentialsTTL   = time.Hour
	sessionAccessKeyPrefix  = "ASIA"
	fallbackAssumedRoleName = "emulated-role"
)

// stsCredentials is the XML shape of AWS STS temporary credentials.
type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

// stsAssumedRoleUser is the XML shape of an STS AssumedRoleUser.
type stsAssumedRoleUser struct {
	Arn           string `xml:"Arn"`
	AssumedRoleID string `xml:"AssumedRoleId"`
}

// assumeRoleWithWebIdentityResponse is the XML response for
// AssumeRoleWithWebIdentity.
type assumeRoleWithWebIdentityResponse struct {
	XMLName         xml.Name           `xml:"AssumeRoleWithWebIdentityResponse"`
	Xmlns           string             `xml:"xmlns,attr"`
	Credentials     stsCredentials     `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	AssumedRoleUser stsAssumedRoleUser `xml:"AssumeRoleWithWebIdentityResult>AssumedRoleUser"`
	RequestID       string             `xml:"ResponseMetadata>RequestId"`
}

// getCallerIdentityResponse is the XML response for GetCallerIdentity.
type getCallerIdentityResponse struct {
	XMLName   xml.Name `xml:"GetCallerIdentityResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	Arn       string   `xml:"GetCallerIdentityResult>Arn"`
	UserID    string   `xml:"GetCallerIdentityResult>UserId"`
	Account   string   `xml:"GetCallerIdentityResult>Account"`
	RequestID string   `xml:"ResponseMetadata>RequestId"`
}

// stsErrorResponse is the XML shape of an STS error response.
type stsErrorResponse struct {
	XMLName   xml.Name `xml:"ErrorResponse"`
	Error     stsError `xml:"Error"`
	RequestID string   `xml:"RequestId"`
}

// stsError is the XML shape of an STS error's Error element.
type stsError struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// dispatchSTS routes an AWS STS Query protocol request to the appropriate
// handler based on the Action form parameter. Signatures are never
// verified; see resolveAccessKeyID for how a caller's principal is resolved
// on a best-effort basis from the SigV4 credential it presents.
func (s *Server) dispatchSTS(w http.ResponseWriter, r *http.Request) {
	form, err := parseSTSRequest(r)
	if err != nil {
		writeSTSError(w, http.StatusBadRequest, "InvalidParameterValue", "failed to parse request")

		return
	}

	switch action := form.Get("Action"); action {
	case actionAssumeRoleWithWebIdentity:
		s.handleAssumeRoleWithWebIdentity(w, form)
	case actionGetCallerIdentity:
		s.handleGetCallerIdentity(w, r)
	default:
		writeSTSError(w, http.StatusBadRequest, stsErrorInvalidAction, fmt.Sprintf("The action %q is not valid for this web service", action))
	}
}

// handleAssumeRoleWithWebIdentity handles the AssumeRoleWithWebIdentity
// action. It is called unsigned (the caller has no IAM credentials yet), so
// the assumed-role identity is derived directly from the RoleArn parameter
// rather than resolved through the principal store.
func (s *Server) handleAssumeRoleWithWebIdentity(w http.ResponseWriter, form url.Values) {
	roleARN := form.Get("RoleArn")
	sessionName := form.Get("RoleSessionName")

	if roleARN == "" {
		writeSTSError(w, http.StatusBadRequest, stsErrorMissingParameter, "RoleArn is required")

		return
	}

	if sessionName == "" {
		writeSTSError(w, http.StatusBadRequest, stsErrorMissingParameter, "RoleSessionName is required")

		return
	}

	credentials, err := newCredentials(time.Now())
	if err != nil {
		writeSTSError(w, http.StatusInternalServerError, stsErrorInternalFailure, err.Error())

		return
	}

	assumedRoleID, err := newAssumedRoleID(sessionName)
	if err != nil {
		writeSTSError(w, http.StatusInternalServerError, stsErrorInternalFailure, err.Error())

		return
	}

	requestID, err := newRequestID()
	if err != nil {
		writeSTSError(w, http.StatusInternalServerError, stsErrorInternalFailure, err.Error())

		return
	}

	roleUserARN := assumedRoleARN(roleARN, sessionName)

	// Record the session so a GetCallerIdentity signed with these temporary
	// credentials resolves to the assumed role instead of the account root.
	s.sessions.put(credentials.AccessKeyID, callerIdentityResult{
		arn:     roleUserARN,
		userID:  assumedRoleID,
		account: AccountID,
	})

	writeSTSResponse(w, assumeRoleWithWebIdentityResponse{
		Xmlns:       stsXMLNS,
		Credentials: *credentials,
		AssumedRoleUser: stsAssumedRoleUser{
			Arn:           roleUserARN,
			AssumedRoleID: assumedRoleID,
		},
		RequestID: requestID,
	})
}

// handleGetCallerIdentity handles the GetCallerIdentity action, resolving
// the caller's principal from its SigV4 access key ID if the request
// carries one, and falling back to the account root identity otherwise.
func (s *Server) handleGetCallerIdentity(w http.ResponseWriter, r *http.Request) {
	identity, err := s.callerIdentity(r.Context(), r.Form, r.Header)
	if err != nil {
		writeSTSError(w, http.StatusInternalServerError, stsErrorInternalFailure, err.Error())

		return
	}

	requestID, err := newRequestID()
	if err != nil {
		writeSTSError(w, http.StatusInternalServerError, stsErrorInternalFailure, err.Error())

		return
	}

	writeSTSResponse(w, getCallerIdentityResponse{
		Xmlns:     stsXMLNS,
		Arn:       identity.arn,
		UserID:    identity.userID,
		Account:   identity.account,
		RequestID: requestID,
	})
}

// callerIdentityResult is the resolved identity behind a GetCallerIdentity
// call.
type callerIdentityResult struct {
	arn     string
	userID  string
	account string
}

// callerIdentity resolves the identity behind a request, using the access
// key ID from its Authorization header or X-Amz-Credential parameter to
// look it up in the principal store. Requests carrying no recognizable
// access key, or one the store does not know about, resolve to the fjord
// account's root identity.
func (s *Server) callerIdentity(ctx context.Context, form url.Values, header http.Header) (*callerIdentityResult, error) {
	accessKeyID, ok := resolveAccessKeyID(form, header)
	if !ok {
		return defaultCallerIdentity(), nil
	}

	// Temporary session credentials (issued by AssumeRoleWithWebIdentity)
	// resolve to their assumed-role identity, matching IRSA on real EKS.
	if identity, ok := s.sessions.get(accessKeyID); ok {
		return &identity, nil
	}

	principal, err := s.store.GetByAccessKeyID(ctx, accessKeyID)
	if errors.Is(err, ErrPrincipalNotFound) {
		return defaultCallerIdentity(), nil
	}

	if err != nil {
		return nil, fmt.Errorf("resolve principal for access key %q: %w", accessKeyID, err)
	}

	return &callerIdentityResult{
		arn:     principal.ARN,
		userID:  "AIDA" + principal.AccessKeyID,
		account: AccountID,
	}, nil
}

// defaultCallerIdentity is the identity GetCallerIdentity reports for
// requests it cannot resolve to a registered principal.
func defaultCallerIdentity() *callerIdentityResult {
	return &callerIdentityResult{
		arn:     fmt.Sprintf("arn:aws:iam::%s:root", AccountID),
		userID:  AccountID,
		account: AccountID,
	}
}

// parseSTSRequest parses r's query string and application/x-www-form-urlencoded
// body into a single url.Values, as AWS's Query protocol requires (the
// Action and every other parameter may arrive in either place).
func parseSTSRequest(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}

	return r.Form, nil
}

// accessKeyIDFromCredentialScope extracts the access key ID from a SigV4
// credential scope of the form
// "<access-key>/<date>/<region>/<service>/aws4_request".
func accessKeyIDFromCredentialScope(scope string) (string, bool) {
	accessKeyID, _, ok := strings.Cut(scope, "/")
	if !ok || accessKeyID == "" {
		return "", false
	}

	return accessKeyID, true
}

// accessKeyIDFromAuthorizationHeader extracts the access key ID from a
// SigV4 Authorization header of the form "AWS4-HMAC-SHA256
// Credential=<access-key>/<date>/<region>/<service>/aws4_request,
// SignedHeaders=..., Signature=...".
func accessKeyIDFromAuthorizationHeader(header string) (string, bool) {
	const authFieldPrefix = "Credential="

	idx := strings.Index(header, authFieldPrefix)
	if idx < 0 {
		return "", false
	}

	scope := header[idx+len(authFieldPrefix):]
	if comma := strings.IndexByte(scope, ','); comma >= 0 {
		scope = scope[:comma]
	}

	return accessKeyIDFromCredentialScope(strings.TrimSpace(scope))
}

// resolveAccessKeyID extracts the caller's SigV4 access key ID from a
// request's Authorization header or X-Amz-Credential form parameter,
// without verifying the accompanying signature.
func resolveAccessKeyID(form url.Values, header http.Header) (string, bool) {
	if auth := header.Get("Authorization"); auth != "" {
		if accessKeyID, ok := accessKeyIDFromAuthorizationHeader(auth); ok {
			return accessKeyID, true
		}
	}

	if cred := form.Get("X-Amz-Credential"); cred != "" {
		return accessKeyIDFromCredentialScope(cred)
	}

	return "", false
}

// roleARNParts extracts the partition and role name from an IAM role ARN
// (arn:<partition>:iam::<account>:role/<path.../><name>). Unparseable input
// falls back to partition "aws" and role name fallbackAssumedRoleName so
// AssumeRoleWithWebIdentity stays permissive.
func roleARNParts(roleARN string) (partition, roleName string) {
	partition, roleName = "aws", fallbackAssumedRoleName

	parts := strings.SplitN(roleARN, ":", 6)
	if len(parts) != 6 || parts[2] != "iam" {
		return partition, roleName
	}

	if parts[1] != "" {
		partition = parts[1]
	}

	resource, ok := strings.CutPrefix(parts[5], "role/")
	if !ok || resource == "" {
		return partition, roleName
	}

	if name := path.Base(resource); name != "." && name != "/" {
		roleName = name
	}

	return partition, roleName
}

// assumedRoleARN converts an IAM role ARN and session name into the STS
// assumed-role ARN AssumeRoleWithWebIdentity returns, e.g.
// "arn:aws:iam::123456789012:role/my-role" + "session" ->
// "arn:aws:sts::000000000000:assumed-role/my-role/session". The account is
// always fjord's dummy AccountID, matching every other ARN fjord issues.
func assumedRoleARN(roleARN, sessionName string) string {
	partition, roleName := roleARNParts(roleARN)

	return fmt.Sprintf("arn:%s:sts::%s:assumed-role/%s/%s", partition, AccountID, roleName, sessionName)
}

// newCredentials returns dummy temporary credentials expiring one hour
// after now.
func newCredentials(now time.Time) (*stsCredentials, error) {
	accessKeyID, err := randomHexString(8)
	if err != nil {
		return nil, fmt.Errorf("generate session access key id: %w", err)
	}

	secretAccessKey, err := randomHexString(20)
	if err != nil {
		return nil, fmt.Errorf("generate session secret access key: %w", err)
	}

	sessionToken, err := randomHexString(40)
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}

	return &stsCredentials{
		AccessKeyID:     sessionAccessKeyPrefix + accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
		Expiration:      now.Add(sessionCredentialsTTL).UTC().Format(time.RFC3339),
	}, nil
}

// newRequestID returns a random request ID for an STS response.
func newRequestID() (string, error) {
	id, err := randomHexString(16)
	if err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}

	return id, nil
}

// newAssumedRoleID returns a random AssumedRoleId for sessionName, shaped
// like AWS's own ("AROA<unique-id>:<session-name>").
func newAssumedRoleID(sessionName string) (string, error) {
	id, err := randomHexString(6)
	if err != nil {
		return "", fmt.Errorf("generate assumed role id: %w", err)
	}

	return fmt.Sprintf("AROA%s:%s", strings.ToUpper(id), sessionName), nil
}

// randomHexString returns a random hex-encoded string of n bytes (2n
// characters).
func randomHexString(n int) (string, error) {
	buf := make([]byte, n)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// errorType returns the STS error Type ("Sender" or "Receiver") for an HTTP
// status code, matching AWS's client/server error classification.
func errorType(status int) string {
	if status >= http.StatusInternalServerError {
		return "Receiver"
	}

	return "Sender"
}

// writeSTSResponse encodes v as the XML body of a successful STS response.
func writeSTSResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

// writeSTSError writes an STS-shaped ErrorResponse with status and code.
func writeSTSError(w http.ResponseWriter, status int, code, message string) {
	requestID, _ := newRequestID()

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(stsErrorResponse{
		Error: stsError{
			Type:    errorType(status),
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	})
}
