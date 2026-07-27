package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTokenTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  string
		want    time.Duration
		wantErr bool
	}{
		{name: "valid ttl", header: "21600", want: 21600 * time.Second},
		{name: "minimum ttl", header: "1", want: time.Second},
		{name: "missing header", header: "", wantErr: true},
		{name: "not a number", header: "abc", wantErr: true},
		{name: "zero is out of range", header: "0", wantErr: true},
		{name: "too large", header: "21601", wantErr: true},
		{name: "negative", header: "-1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseTokenTTL(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTokenTTL(%q) error = %v, wantErr %v", tt.header, err, tt.wantErr)
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("parseTokenTTL(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestIsLatestVersionPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "exact latest", path: "/latest", want: true},
		{name: "latest subpath", path: "/latest/meta-data/", want: true},
		{name: "unsupported version", path: "/2016-09-02/meta-data/", want: false},
		{name: "root", path: "/", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isLatestVersionPath(tt.path); got != tt.want {
				t.Errorf("isLatestVersionPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestCredentialsExpired(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		exp  string
		want bool
	}{
		{name: "future expiration", exp: now.Add(time.Hour).Format(time.RFC3339), want: false},
		{name: "past expiration", exp: now.Add(-time.Hour).Format(time.RFC3339), want: true},
		{name: "exactly now", exp: now.Format(time.RFC3339), want: true},
		{name: "unparsable", exp: "not-a-time", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			creds := &stsCredentials{Expiration: tt.exp}
			if got := credentialsExpired(creds, now); got != tt.want {
				t.Errorf("credentialsExpired(%q) = %v, want %v", tt.exp, got, tt.want)
			}
		})
	}
}

func TestNewInstanceID(t *testing.T) {
	t.Parallel()

	id, err := newInstanceID()
	if err != nil {
		t.Fatalf("newInstanceID() error = %v", err)
	}

	if !strings.HasPrefix(id, "i-") || len(id) <= len("i-") {
		t.Errorf("newInstanceID() = %q, want an \"i-\"-prefixed string", id)
	}
}

func TestIMDS_TokenRequiredForMetadataGET(t *testing.T) {
	t.Parallel()

	imds, err := NewIMDS(NewInMemoryPrincipalStore(), "")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/placement/region", http.NoBody)
	imds.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestIMDS_UnsupportedVersionPath(t *testing.T) {
	t.Parallel()

	imds, err := NewIMDS(NewInMemoryPrincipalStore(), "")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/2016-09-02/meta-data/placement/region", http.NoBody)
	imds.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestIMDS_TokenAndRegionRoundTrip(t *testing.T) {
	t.Parallel()

	imds, err := NewIMDS(NewInMemoryPrincipalStore(), "")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	handler := imds.Handler()
	token := issueTestToken(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/placement/region", http.NoBody)
	req.Header.Set(imdsSessionHeaderName, token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if got := rec.Body.String(); got != imdsRegion {
		t.Errorf("region = %q, want %q", got, imdsRegion)
	}
}

// TestIMDS_SecurityCredentialsFlow exercises the full IMDSv2 flow a real
// pod's AWS SDK follows to obtain node-role credentials: PUT a token, list
// the role, fetch its credentials, and verify those credentials resolve to
// the node role's assumed-role ARN via the same principal store
// fjord-agent's fake STS uses for GetCallerIdentity.
func TestIMDS_SecurityCredentialsFlow(t *testing.T) {
	t.Parallel()

	store := NewInMemoryPrincipalStore()

	imds, err := NewIMDS(store, "my-node-role")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	handler := imds.Handler()
	token := issueTestToken(t, handler)

	roleList := doTokenedGET(t, handler, "/latest/meta-data/iam/security-credentials/", token)
	if roleList != "my-node-role" {
		t.Errorf("role list = %q, want %q", roleList, "my-node-role")
	}

	credsBody := doTokenedGET(t, handler, "/latest/meta-data/iam/security-credentials/my-node-role", token)

	var doc imdsCredentialsDocument
	if err := json.Unmarshal([]byte(credsBody), &doc); err != nil {
		t.Fatalf("unmarshal credentials: %v\n%s", err, credsBody)
	}

	if doc.Code != "Success" || doc.Type != "AWS-HMAC" {
		t.Errorf("credentials document = %+v, want Code=Success Type=AWS-HMAC", doc)
	}

	if doc.AccessKeyID == "" || doc.SecretAccessKey == "" || doc.Token == "" {
		t.Fatalf("credentials document has empty fields: %+v", doc)
	}

	principal, err := store.GetByAccessKeyID(context.Background(), doc.AccessKeyID)
	if err != nil {
		t.Fatalf("store.GetByAccessKeyID(%q) error = %v", doc.AccessKeyID, err)
	}

	wantARN := "arn:aws:sts::" + AccountID + ":assumed-role/my-node-role/" + imds.instanceID
	if principal.ARN != wantARN {
		t.Errorf("principal ARN = %q, want %q", principal.ARN, wantARN)
	}

	// Fetching credentials again before expiration must return the same
	// cached access key instead of registering a new one every call.
	credsBody2 := doTokenedGET(t, handler, "/latest/meta-data/iam/security-credentials/my-node-role", token)

	var doc2 imdsCredentialsDocument
	if err := json.Unmarshal([]byte(credsBody2), &doc2); err != nil {
		t.Fatalf("unmarshal credentials: %v\n%s", err, credsBody2)
	}

	if doc2.AccessKeyID != doc.AccessKeyID {
		t.Errorf("second fetch access key = %q, want unchanged %q", doc2.AccessKeyID, doc.AccessKeyID)
	}
}

func TestIMDS_UnknownRoleNotFound(t *testing.T) {
	t.Parallel()

	imds, err := NewIMDS(NewInMemoryPrincipalStore(), "my-node-role")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	handler := imds.Handler()
	token := issueTestToken(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/iam/security-credentials/not-my-role", http.NoBody)
	req.Header.Set(imdsSessionHeaderName, token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestIMDS_InstanceIdentityDocument(t *testing.T) {
	t.Parallel()

	imds, err := NewIMDS(NewInMemoryPrincipalStore(), "")
	if err != nil {
		t.Fatalf("NewIMDS() error = %v", err)
	}

	handler := imds.Handler()
	token := issueTestToken(t, handler)

	body := doTokenedGET(t, handler, "/latest/dynamic/instance-identity/document", token)

	var doc instanceIdentityDocument
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal instance identity document: %v\n%s", err, body)
	}

	if doc.AccountID != AccountID {
		t.Errorf("accountId = %q, want %q", doc.AccountID, AccountID)
	}

	if doc.Region != imdsRegion {
		t.Errorf("region = %q, want %q", doc.Region, imdsRegion)
	}

	if doc.InstanceID != imds.instanceID {
		t.Errorf("instanceId = %q, want %q", doc.InstanceID, imds.instanceID)
	}
}

// issueTestToken performs the PUT /latest/api/token step of the IMDSv2 flow
// against handler and returns the issued token.
func issueTestToken(t *testing.T, handler http.Handler) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/latest/api/token", http.NoBody)
	req.Header.Set(imdsSessionTTLHeaderName, "21600")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT token status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	token := rec.Body.String()
	if token == "" {
		t.Fatal("PUT token returned an empty token")
	}

	return token
}

// doTokenedGET issues a GET to path against handler, carrying token in
// imdsSessionHeaderName, and returns the response body. It fails t if the
// response status is not 200.
func doTokenedGET(t *testing.T, handler http.Handler, path, token string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set(imdsSessionHeaderName, token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
	}

	return rec.Body.String()
}
