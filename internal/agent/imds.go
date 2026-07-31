package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultNodeRoleName is the IAM role name IMDS advertises as fjord's
	// node instance role when no --node-role-name flag overrides it.
	DefaultNodeRoleName = "fjord-node-role"

	// imdsRegion, imdsAvailabilityZone, imdsInstanceType, and imdsImageID
	// are the dummy EC2 placement/identity values IMDS reports. fjord
	// clusters are not tied to any real AWS region or instance, so these
	// are fixed, plausible-looking values.
	imdsRegion           = "us-east-1"
	imdsAvailabilityZone = "us-east-1a"
	imdsInstanceType     = "t3.medium"
	imdsImageID          = "ami-0000000000000000"

	imdsSessionHeaderName    = "X-aws-ec2-metadata-token"
	imdsSessionTTLHeaderName = "X-aws-ec2-metadata-token-ttl-seconds"
	imdsTokenTTLMin          = 1
	imdsTokenTTLMax          = 21600

	imdsTokenBytes      = 20
	imdsInstanceIDBytes = 8
)

// IMDS is the fjord-agent HTTP server emulating the subset of the EC2
// Instance Metadata Service (IMDSv2 only) the AWS SDK's default credential
// chain and region resolution fall back to: the token exchange, the IAM
// security-credentials role and its temporary credentials, and region/AZ/
// instance-identity metadata. It lets any pod with no
// eks.amazonaws.com/role-arn annotation obtain node-role credentials as if
// fjord's nodes were real EC2 instances running with an instance
// profile.
type IMDS struct {
	store      PrincipalStore
	roleName   string
	instanceID string

	tokenMu sync.Mutex
	tokens  map[string]time.Time

	credMu      sync.Mutex
	credentials *stsCredentials
}

// NewIMDS returns an IMDS advertising roleName (DefaultNodeRoleName if
// empty) as its single IAM role, registering the credentials it issues in
// store so a GetCallerIdentity call resolves them (see
// internal/agent/sts.go's callerIdentity, which looks up an access key ID
// in the same store fjord-agent's fake STS uses). store and fjord-agent's
// fake STS must therefore be backed by the same fjord-principals Secret;
// IMDS runs in a separate process (a DaemonSet, one per node) from the fake
// STS's Deployment, so the in-memory sessionStore AssumeRoleWithWebIdentity
// uses cannot be shared directly across that process boundary.
func NewIMDS(store PrincipalStore, roleName string) (*IMDS, error) {
	if roleName == "" {
		roleName = DefaultNodeRoleName
	}

	instanceID, err := newInstanceID()
	if err != nil {
		return nil, fmt.Errorf("create imds: %w", err)
	}

	return &IMDS{
		store:      store,
		roleName:   roleName,
		instanceID: instanceID,
		tokens:     make(map[string]time.Time),
	}, nil
}

// Handler returns the http.Handler serving every IMDSv2 path IMDS supports.
func (m *IMDS) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /latest/api/token", m.handleToken)
	mux.HandleFunc("GET /latest/meta-data/iam/security-credentials/", m.withToken(m.handleRoleList))
	mux.HandleFunc("GET /latest/meta-data/iam/security-credentials/{role}", m.withToken(m.handleRoleCredentials))
	mux.HandleFunc("GET /latest/meta-data/placement/region", m.withToken(writePlainText(imdsRegion)))
	mux.HandleFunc("GET /latest/meta-data/placement/availability-zone", m.withToken(writePlainText(imdsAvailabilityZone)))
	mux.HandleFunc("GET /latest/dynamic/instance-identity/document", m.withToken(m.handleInstanceIdentityDocument))
	mux.HandleFunc("/", handleFallback)

	return mux
}

// handleToken serves PUT /latest/api/token: it validates the caller's
// requested TTL and issues a new session token, valid for that TTL, that
// every subsequent GET must present in imdsSessionHeaderName.
func (m *IMDS) handleToken(w http.ResponseWriter, r *http.Request) {
	ttl, err := parseTokenTTL(r.Header.Get(imdsSessionTTLHeaderName))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	token, err := m.issueToken(ttl)
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, token)
}

// handleRoleList serves the trailing-slash security-credentials path,
// listing IMDS's single role name.
func (m *IMDS) handleRoleList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, m.roleName)
}

// handleRoleCredentials serves a single role's temporary credentials,
// generating and registering them in m.store on first request (or once the
// previously issued ones expire).
func (m *IMDS) handleRoleCredentials(w http.ResponseWriter, r *http.Request) {
	if role := r.PathValue("role"); role != m.roleName {
		http.NotFound(w, r)

		return
	}

	creds, err := m.credentialsFor(r.Context())
	if err != nil {
		http.Error(w, "failed to generate credentials", http.StatusInternalServerError)

		return
	}

	doc := credentialsDocument(creds, time.Now())

	body, err := json.Marshal(&doc)
	if err != nil {
		http.Error(w, "failed to encode credentials", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleInstanceIdentityDocument serves the dynamic instance-identity
// document.
func (m *IMDS) handleInstanceIdentityDocument(w http.ResponseWriter, _ *http.Request) {
	body, err := json.Marshal(instanceIdentityDocument{
		AccountID:        AccountID,
		AvailabilityZone: imdsAvailabilityZone,
		ImageID:          imdsImageID,
		InstanceID:       m.instanceID,
		InstanceType:     imdsInstanceType,
		Region:           imdsRegion,
	})
	if err != nil {
		http.Error(w, "failed to encode instance identity document", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// credentialsFor returns IMDS's currently valid node-role credentials,
// generating (and registering in m.store, keyed by the fresh access key ID,
// under the assumed-role ARN GetCallerIdentity must resolve it to) a new
// set if none exist yet or the previous ones expired.
func (m *IMDS) credentialsFor(ctx context.Context) (*stsCredentials, error) {
	m.credMu.Lock()
	defer m.credMu.Unlock()

	now := time.Now()

	if m.credentials != nil && !credentialsExpired(m.credentials, now) {
		return m.credentials, nil
	}

	creds, err := newCredentials(now)
	if err != nil {
		return nil, fmt.Errorf("generate node role credentials: %w", err)
	}

	principal := Principal{
		AccessKeyID: creds.AccessKeyID,
		ARN:         assumedRoleARN(RoleARN(m.roleName), m.instanceID),
		Name:        m.roleName,
	}

	if err := m.store.Put(ctx, principal); err != nil {
		return nil, fmt.Errorf("register node role credentials: %w", err)
	}

	m.credentials = creds

	return m.credentials, nil
}

// issueToken generates a fresh IMDSv2 session token valid for ttl,
// opportunistically evicting already-expired tokens so m.tokens does not
// grow unbounded over IMDS's lifetime.
func (m *IMDS) issueToken(ttl time.Duration) (string, error) {
	token, err := randomHexString(imdsTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate imds token: %w", err)
	}

	now := time.Now()

	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()

	for existing, expiry := range m.tokens {
		if now.After(expiry) {
			delete(m.tokens, existing)
		}
	}

	m.tokens[token] = now.Add(ttl)

	return token, nil
}

// validToken reports whether token was issued by issueToken and has not yet
// expired.
func (m *IMDS) validToken(token string) bool {
	if token == "" {
		return false
	}

	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()

	expiry, ok := m.tokens[token]
	if !ok {
		return false
	}

	if time.Now().After(expiry) {
		delete(m.tokens, token)

		return false
	}

	return true
}

// withToken wraps next so it only runs when the request carries a valid
// imdsSessionHeaderName, matching IMDSv2's requirement that every metadata GET
// present a token obtained from handleToken. A missing or invalid token
// reports 401, matching real IMDSv2.
func (m *IMDS) withToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.validToken(r.Header.Get(imdsSessionHeaderName)) {
			http.Error(w, "missing or invalid "+imdsSessionHeaderName+" header", http.StatusUnauthorized)

			return
		}

		next(w, r)
	}
}

// handleFallback serves every path Handler's mux does not otherwise match:
// 403 for any path outside /latest (an unsupported metadata service
// version, matching real IMDS), 404 otherwise.
func handleFallback(w http.ResponseWriter, r *http.Request) {
	if !isLatestVersionPath(r.URL.Path) {
		http.Error(w, "unsupported metadata service version", http.StatusForbidden)

		return
	}

	http.NotFound(w, r)
}

// isLatestVersionPath reports whether path falls under the only metadata
// service version IMDS supports, "/latest".
func isLatestVersionPath(path string) bool {
	return path == "/latest" || strings.HasPrefix(path, "/latest/")
}

// parseTokenTTL validates header (the value of imdsSessionTTLHeaderName) and
// returns it as a duration. Real IMDSv2 requires the header on every PUT
// /latest/api/token request and rejects a TTL outside [imdsTokenTTLMin,
// imdsTokenTTLMax] seconds.
func parseTokenTTL(header string) (time.Duration, error) {
	if header == "" {
		return 0, fmt.Errorf("missing %s header", imdsSessionTTLHeaderName)
	}

	seconds, err := strconv.Atoi(header)
	if err != nil {
		return 0, fmt.Errorf("invalid %s header: %w", imdsSessionTTLHeaderName, err)
	}

	if seconds < imdsTokenTTLMin || seconds > imdsTokenTTLMax {
		return 0, fmt.Errorf("%s must be between %d and %d", imdsSessionTTLHeaderName, imdsTokenTTLMin, imdsTokenTTLMax)
	}

	return time.Duration(seconds) * time.Second, nil
}

// credentialsExpired reports whether creds' Expiration has passed as of
// now, or is unparsable (which is treated as expired so a fresh set is
// generated rather than serving credentials that cannot be validated).
func credentialsExpired(creds *stsCredentials, now time.Time) bool {
	expiration, err := time.Parse(time.RFC3339, creds.Expiration)
	if err != nil {
		return true
	}

	return !now.Before(expiration)
}

// newInstanceID returns a random EC2-instance-ID-shaped string ("i-"
// followed by 16 hex characters).
func newInstanceID() (string, error) {
	suffix, err := randomHexString(imdsInstanceIDBytes)
	if err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}

	return "i-" + suffix, nil
}

// imdsCredentialsDocument is the JSON shape of the EC2 IMDS
// security-credentials/<role> response. Its field names are dictated by the
// real AWS API (PascalCase) rather than fjord's own json tag convention, so
// it marshals itself explicitly instead of relying on struct tags.
type imdsCredentialsDocument struct {
	Code            string
	LastUpdated     string
	Type            string
	AccessKeyID     string
	SecretAccessKey string
	Token           string
	Expiration      string
}

// MarshalJSON implements json.Marshaler, encoding d with the exact
// PascalCase field names the real EC2 IMDS API uses.
func (d *imdsCredentialsDocument) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"Code":            d.Code,
		"LastUpdated":     d.LastUpdated,
		"Type":            d.Type,
		"AccessKeyId":     d.AccessKeyID,
		"SecretAccessKey": d.SecretAccessKey,
		"Token":           d.Token,
		"Expiration":      d.Expiration,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal imds credentials document: %w", err)
	}

	return body, nil
}

// credentialsDocument builds the imdsCredentialsDocument for creds, using
// lastUpdated as the LastUpdated timestamp.
func credentialsDocument(creds *stsCredentials, lastUpdated time.Time) imdsCredentialsDocument {
	return imdsCredentialsDocument{
		Code:            "Success",
		LastUpdated:     lastUpdated.UTC().Format(time.RFC3339),
		Type:            "AWS-HMAC",
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		Token:           creds.SessionToken,
		Expiration:      creds.Expiration,
	}
}

// instanceIdentityDocument is the JSON shape of a minimal EC2
// instance-identity document.
type instanceIdentityDocument struct {
	AccountID        string `json:"accountId"`
	AvailabilityZone string `json:"availabilityZone"`
	ImageID          string `json:"imageId"`
	InstanceID       string `json:"instanceId"`
	InstanceType     string `json:"instanceType"`
	Region           string `json:"region"`
}

// writePlainText returns an http.HandlerFunc that writes body as a
// text/plain response, used for the single-value metadata paths.
func writePlainText(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	}
}
