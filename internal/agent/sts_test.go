package agent

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAssumeRoleWithWebIdentityThenGetCallerIdentity is the IRSA fidelity
// check: temporary credentials issued by AssumeRoleWithWebIdentity must, when
// used to sign a later GetCallerIdentity, resolve to the assumed-role identity
// rather than the account root.
func TestAssumeRoleWithWebIdentityThenGetCallerIdentity(t *testing.T) {
	t.Parallel()

	server := NewServer(NewInMemoryPrincipalStore())
	handler := server.Handler()

	assumeForm := url.Values{
		"Action":           {actionAssumeRoleWithWebIdentity},
		"Version":          {"2011-06-15"},
		"RoleArn":          {"arn:aws:iam::000000000000:role/s3-read-role"},
		"RoleSessionName":  {"session1"},
		"WebIdentityToken": {"any-token"},
	}
	assumeResp := postForm(t, handler, assumeForm, nil)

	var assumed assumeRoleWithWebIdentityResponse
	if err := xml.Unmarshal([]byte(assumeResp), &assumed); err != nil {
		t.Fatalf("unmarshal assume response: %v\n%s", err, assumeResp)
	}

	tempAccessKey := assumed.Credentials.AccessKeyID
	if tempAccessKey == "" {
		t.Fatal("assume response has empty temporary access key")
	}

	// Sign the GetCallerIdentity with the temporary access key, as a real SDK
	// would after assuming the role.
	header := http.Header{
		"Authorization": {"AWS4-HMAC-SHA256 Credential=" + tempAccessKey + "/20240101/us-east-1/sts/aws4_request, Signature=x"},
	}
	callerResp := postForm(t, handler, url.Values{
		"Action":  {actionGetCallerIdentity},
		"Version": {"2011-06-15"},
	}, header)

	var caller getCallerIdentityResponse
	if err := xml.Unmarshal([]byte(callerResp), &caller); err != nil {
		t.Fatalf("unmarshal caller response: %v\n%s", err, callerResp)
	}

	want := "arn:aws:sts::000000000000:assumed-role/s3-read-role/session1"
	if caller.Arn != want {
		t.Errorf("GetCallerIdentity Arn = %q, want %q", caller.Arn, want)
	}
}

// postForm posts an AWS Query form to handler and returns the response body.
func postForm(t *testing.T, handler http.Handler, form url.Values, header http.Header) string {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	return rec.Body.String()
}

func TestAccessKeyIDFromCredentialScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		scope  string
		want   string
		wantOK bool
	}{
		{
			name:   "valid scope",
			scope:  "AKIAEXAMPLE/20240101/us-east-1/sts/aws4_request",
			want:   "AKIAEXAMPLE",
			wantOK: true,
		},
		{
			name:   "access key only, no slash",
			scope:  "AKIAEXAMPLE",
			wantOK: false,
		},
		{
			name:   "empty",
			scope:  "",
			wantOK: false,
		},
		{
			name:   "leading slash yields empty access key",
			scope:  "/20240101/us-east-1/sts/aws4_request",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := accessKeyIDFromCredentialScope(tt.scope)
			if ok != tt.wantOK {
				t.Fatalf("accessKeyIDFromCredentialScope(%q) ok = %v, want %v", tt.scope, ok, tt.wantOK)
			}

			if ok && got != tt.want {
				t.Errorf("accessKeyIDFromCredentialScope(%q) = %q, want %q", tt.scope, got, tt.want)
			}
		})
	}
}

func TestAccessKeyIDFromAuthorizationHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{
			name:   "standard sigv4 header",
			header: "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20240101/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=abc123",
			want:   "AKIAEXAMPLE",
			wantOK: true,
		},
		{
			name:   "no credential param",
			header: "Bearer sometoken",
			wantOK: false,
		},
		{
			name:   "empty header",
			header: "",
			wantOK: false,
		},
		{
			name:   "credential with no trailing comma",
			header: "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20240101/us-east-1/sts/aws4_request",
			want:   "AKIAEXAMPLE",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := accessKeyIDFromAuthorizationHeader(tt.header)
			if ok != tt.wantOK {
				t.Fatalf("accessKeyIDFromAuthorizationHeader(%q) ok = %v, want %v", tt.header, ok, tt.wantOK)
			}

			if ok && got != tt.want {
				t.Errorf("accessKeyIDFromAuthorizationHeader(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestResolveAccessKeyID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		form   url.Values
		header http.Header
		want   string
		wantOK bool
	}{
		{
			name:   "authorization header takes precedence",
			form:   url.Values{"X-Amz-Credential": {"AKIAFROMQUERY/20240101/us-east-1/sts/aws4_request"}},
			header: http.Header{"Authorization": {"AWS4-HMAC-SHA256 Credential=AKIAFROMHEADER/20240101/us-east-1/sts/aws4_request, Signature=abc"}},
			want:   "AKIAFROMHEADER",
			wantOK: true,
		},
		{
			name:   "falls back to x-amz-credential",
			form:   url.Values{"X-Amz-Credential": {"AKIAFROMQUERY/20240101/us-east-1/sts/aws4_request"}},
			header: http.Header{},
			want:   "AKIAFROMQUERY",
			wantOK: true,
		},
		{
			name:   "neither present",
			form:   url.Values{},
			header: http.Header{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := resolveAccessKeyID(tt.form, tt.header)
			if ok != tt.wantOK {
				t.Fatalf("resolveAccessKeyID() ok = %v, want %v", ok, tt.wantOK)
			}

			if ok && got != tt.want {
				t.Errorf("resolveAccessKeyID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoleARNParts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		roleARN       string
		wantPartition string
		wantRoleName  string
	}{
		{
			name:          "simple role",
			roleARN:       "arn:aws:iam::123456789012:role/my-role",
			wantPartition: "aws",
			wantRoleName:  "my-role",
		},
		{
			name:          "role with path",
			roleARN:       "arn:aws:iam::123456789012:role/path/to/my-role",
			wantPartition: "aws",
			wantRoleName:  "my-role",
		},
		{
			name:          "china partition",
			roleARN:       "arn:aws-cn:iam::123456789012:role/my-role",
			wantPartition: "aws-cn",
			wantRoleName:  "my-role",
		},
		{
			name:          "malformed arn falls back",
			roleARN:       "not-an-arn",
			wantPartition: "aws",
			wantRoleName:  fallbackAssumedRoleName,
		},
		{
			name:          "not an iam arn falls back",
			roleARN:       "arn:aws:sts::123456789012:role/my-role",
			wantPartition: "aws",
			wantRoleName:  fallbackAssumedRoleName,
		},
		{
			name:          "empty resource falls back to default role name",
			roleARN:       "arn:aws:iam::123456789012:role/",
			wantPartition: "aws",
			wantRoleName:  fallbackAssumedRoleName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPartition, gotRoleName := roleARNParts(tt.roleARN)
			if gotPartition != tt.wantPartition {
				t.Errorf("roleARNParts(%q) partition = %q, want %q", tt.roleARN, gotPartition, tt.wantPartition)
			}

			if gotRoleName != tt.wantRoleName {
				t.Errorf("roleARNParts(%q) roleName = %q, want %q", tt.roleARN, gotRoleName, tt.wantRoleName)
			}
		})
	}
}

func TestAssumedRoleARN(t *testing.T) {
	t.Parallel()

	got := assumedRoleARN("arn:aws:iam::123456789012:role/my-role", "my-session")

	want := "arn:aws:sts::" + AccountID + ":assumed-role/my-role/my-session"
	if got != want {
		t.Errorf("assumedRoleARN() = %q, want %q", got, want)
	}
}

func TestNewCredentials(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	creds, err := newCredentials(now)
	if err != nil {
		t.Fatalf("newCredentials() error = %v", err)
	}

	if !strings.HasPrefix(creds.AccessKeyID, sessionAccessKeyPrefix) {
		t.Errorf("AccessKeyId = %q, want prefix %q", creds.AccessKeyID, sessionAccessKeyPrefix)
	}

	wantExpiration := now.Add(time.Hour).UTC().Format(time.RFC3339)
	if creds.Expiration != wantExpiration {
		t.Errorf("Expiration = %q, want %q", creds.Expiration, wantExpiration)
	}

	if _, err := time.Parse(time.RFC3339, creds.Expiration); err != nil {
		t.Errorf("Expiration = %q is not RFC3339: %v", creds.Expiration, err)
	}

	if creds.SecretAccessKey == "" || creds.SessionToken == "" {
		t.Errorf("SecretAccessKey/SessionToken must not be empty: %+v", creds)
	}
}

func TestErrorType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   string
	}{
		{status: http.StatusBadRequest, want: "Sender"},
		{status: http.StatusInternalServerError, want: "Receiver"},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			t.Parallel()

			if got := errorType(tt.status); got != tt.want {
				t.Errorf("errorType(%d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestServer_AssumeRoleWithWebIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantCode   string
		wantArn    string
	}{
		{
			name: "success",
			form: url.Values{
				"Action":           {actionAssumeRoleWithWebIdentity},
				"Version":          {"2011-06-15"},
				"RoleArn":          {"arn:aws:iam::123456789012:role/my-role"},
				"RoleSessionName":  {"my-session"},
				"WebIdentityToken": {"eyJhbGciOiJSUzI1NiJ9.example"},
			},
			wantStatus: http.StatusOK,
			wantArn:    "arn:aws:sts::" + AccountID + ":assumed-role/my-role/my-session",
		},
		{
			name: "missing role arn",
			form: url.Values{
				"Action":          {actionAssumeRoleWithWebIdentity},
				"RoleSessionName": {"my-session"},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   stsErrorMissingParameter,
		},
		{
			name: "missing session name",
			form: url.Values{
				"Action":  {actionAssumeRoleWithWebIdentity},
				"RoleArn": {"arn:aws:iam::123456789012:role/my-role"},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   stsErrorMissingParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assertAssumeRoleWithWebIdentity(t, tt.form, tt.wantStatus, tt.wantCode, tt.wantArn)
		})
	}
}

// assertAssumeRoleWithWebIdentity posts form to a fresh Server and checks
// the response matches wantStatus and, depending on wantStatus, either
// wantCode (error responses) or wantArn (successful responses).
func assertAssumeRoleWithWebIdentity(t *testing.T, form url.Values, wantStatus int, wantCode, wantArn string) {
	t.Helper()

	server := NewServer(NewInMemoryPrincipalStore())
	rec := doSTSRequest(t, server, form, nil)

	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}

	if wantStatus != http.StatusOK {
		var errResp stsErrorResponse

		mustUnmarshalXML(t, rec.Body.Bytes(), &errResp)

		if errResp.Error.Code != wantCode {
			t.Errorf("error code = %q, want %q", errResp.Error.Code, wantCode)
		}

		return
	}

	var resp assumeRoleWithWebIdentityResponse

	mustUnmarshalXML(t, rec.Body.Bytes(), &resp)

	if resp.Xmlns != stsXMLNS {
		t.Errorf("xmlns = %q, want %q", resp.Xmlns, stsXMLNS)
	}

	if resp.AssumedRoleUser.Arn != wantArn {
		t.Errorf("AssumedRoleUser.Arn = %q, want %q", resp.AssumedRoleUser.Arn, wantArn)
	}

	if resp.Credentials.AccessKeyID == "" {
		t.Error("Credentials.AccessKeyId is empty")
	}
}

func TestServer_GetCallerIdentity(t *testing.T) {
	t.Parallel()

	registered := Principal{AccessKeyID: "AKIAALICE", ARN: PrincipalARN("alice"), Name: "alice"}
	rootArn := "arn:aws:iam::" + AccountID + ":root"

	tests := []struct {
		name       string
		registered *Principal
		header     http.Header
		wantArn    string
	}{
		{
			name:       "resolves registered principal via authorization header",
			registered: &registered,
			header:     http.Header{"Authorization": {"AWS4-HMAC-SHA256 Credential=AKIAALICE/20240101/us-east-1/sts/aws4_request, Signature=abc"}},
			wantArn:    registered.ARN,
		},
		{
			name:    "unresolvable access key falls back to root identity",
			header:  http.Header{"Authorization": {"AWS4-HMAC-SHA256 Credential=AKIAUNKNOWN/20240101/us-east-1/sts/aws4_request, Signature=abc"}},
			wantArn: rootArn,
		},
		{
			name:    "no credentials falls back to root identity",
			wantArn: rootArn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assertGetCallerIdentity(t, tt.registered, tt.header, tt.wantArn)
		})
	}
}

// assertGetCallerIdentity registers registered (if non-nil) in a fresh
// Server's principal store, sends it a GetCallerIdentity request carrying
// header, and checks the response's Arn matches wantArn.
func assertGetCallerIdentity(t *testing.T, registered *Principal, header http.Header, wantArn string) {
	t.Helper()

	store := NewInMemoryPrincipalStore()
	if registered != nil {
		if err := store.Put(t.Context(), *registered); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}

	server := NewServer(store)
	rec := doSTSRequest(t, server, url.Values{"Action": {actionGetCallerIdentity}}, header)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp getCallerIdentityResponse

	mustUnmarshalXML(t, rec.Body.Bytes(), &resp)

	if resp.Arn != wantArn {
		t.Errorf("Arn = %q, want %q", resp.Arn, wantArn)
	}

	if resp.Account != AccountID {
		t.Errorf("Account = %q, want %q", resp.Account, AccountID)
	}
}

func TestServer_UnknownAction(t *testing.T) {
	t.Parallel()

	server := NewServer(NewInMemoryPrincipalStore())
	rec := doSTSRequest(t, server, url.Values{"Action": {"DeleteEverything"}}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var errResp stsErrorResponse

	mustUnmarshalXML(t, rec.Body.Bytes(), &errResp)

	if errResp.Error.Code != stsErrorInvalidAction {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, stsErrorInvalidAction)
	}
}

// doSTSRequest posts form as an application/x-www-form-urlencoded body to
// server's Handler, merging in header if non-nil, and returns the recorded
// response.
func doSTSRequest(t *testing.T, server *Server, form url.Values, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// mustUnmarshalXML decodes body into v, failing t on error.
func mustUnmarshalXML(t *testing.T, body []byte, v any) {
	t.Helper()

	if err := xml.Unmarshal(body, v); err != nil {
		t.Fatalf("xml.Unmarshal() error = %v, body: %s", err, body)
	}
}
