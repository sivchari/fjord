package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/logger"
)

// accessPolicyARNs maps the short policy names `fjord grant access-entry
// --policy` accepts to the standard EKS access policy ARNs
// AssociateAccessPolicy understands (see agent.StandardAccessPolicy*).
var accessPolicyARNs = map[string]string{
	"ClusterAdmin": agent.StandardAccessPolicyClusterAdmin,
	"Admin":        agent.StandardAccessPolicyAdmin,
	"Edit":         agent.StandardAccessPolicyEdit,
	"View":         agent.StandardAccessPolicyView,
}

// createAccessEntryRequest mirrors internal/agent's unexported
// createAccessEntryRequest; it is redeclared here (rather than importing
// the unexported internal type) because this command only needs to encode
// it as JSON when calling the EKS API facade over HTTP.
type createAccessEntryRequest struct {
	PrincipalARN     string   `json:"principalArn"`
	Username         string   `json:"username,omitempty"`
	KubernetesGroups []string `json:"kubernetesGroups,omitempty"`
}

// accessScopeRequest mirrors internal/agent's unexported accessScopeRequest.
type accessScopeRequest struct {
	Type       string   `json:"type"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// associateAccessPolicyRequest mirrors internal/agent's unexported
// associateAccessPolicyRequest.
type associateAccessPolicyRequest struct {
	PolicyARN   string             `json:"policyArn"`
	AccessScope accessScopeRequest `json:"accessScope"`
}

// facadeErrorResponse mirrors the {"message": ...} body
// internal/agent's writeEKSAuthError writes on failure.
type facadeErrorResponse struct {
	Message string `json:"message"`
}

// accessEntryOptions carries `fjord grant access-entry`'s flag values.
type accessEntryOptions struct {
	principalRegistryOptions
	principal string
	policy    string
	namespace string
	username  string
	groups    []string
	hostPort  int32
}

func newGrantCmd(logger logger.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Grant a resource",
	}
	cmd.AddCommand(newGrantAccessEntryCmd(logger))

	return cmd
}

func newGrantAccessEntryCmd(_ logger.Logger) *cobra.Command {
	opts := &accessEntryOptions{}

	cmd := &cobra.Command{
		Use:   "access-entry",
		Short: "Register a principal's Kubernetes identity and grant it a standard EKS access policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGrantAccessEntry(cmd, opts)
		},
	}

	opts.registerFlags(cmd)
	cmd.Flags().StringVar(&opts.principal, "principal", "", "principal name registered via \"fjord create principal\" (or a node role name)")
	cmd.Flags().StringVar(&opts.policy, "policy", "", "standard EKS access policy to grant: ClusterAdmin, Admin, Edit, or View")
	cmd.Flags().StringVar(&opts.namespace, "namespace", "", "namespace to scope the policy to (default: cluster-wide)")
	cmd.Flags().StringVar(&opts.username, "username", "", "Kubernetes username the principal authenticates as (default: its principal ARN)")
	cmd.Flags().StringSliceVar(&opts.groups, "group", nil, "additional Kubernetes group(s) the principal authenticates as")
	cmd.Flags().Int32Var(&opts.hostPort, "agent-host-port", defaultAgentHostPort,
		"host port fjord-agent's EKS API facade is published on (default: 48080 for --provider kind, 30080 for --provider rask)")

	_ = cmd.MarkFlagRequired("principal")
	_ = cmd.MarkFlagRequired("policy")

	return cmd
}

func runGrantAccessEntry(cmd *cobra.Command, opts *accessEntryOptions) error {
	policyARN, ok := accessPolicyARNs[opts.policy]
	if !ok {
		return fmt.Errorf("unknown policy %q, want one of ClusterAdmin, Admin, Edit, View", opts.policy)
	}

	client, err := opts.client()
	if err != nil {
		return err
	}

	principal, err := agent.NewSecretPrincipalStore(client).GetByName(cmd.Context(), opts.principal)
	if err != nil {
		return fmt.Errorf("resolve principal %q: %w", opts.principal, err)
	}

	hostPort := resolveAgentHostPort(cmd.Flags().Changed("agent-host-port"), opts.provider, opts.hostPort)
	baseURL := fmt.Sprintf("http://localhost:%d/clusters/%s", hostPort, opts.clusterName)

	if err := createAccessEntry(cmd.Context(), baseURL, principal.ARN, opts.username, opts.groups); err != nil {
		return err
	}

	if err := associateAccessPolicy(cmd.Context(), baseURL, principal.ARN, policyARN, opts.namespace); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Granted %s access to %q for principal %q (%s)\n",
		opts.policy, opts.clusterName, opts.principal, principal.ARN)

	return nil
}

// createAccessEntry registers principalARN's Kubernetes identity via
// POST /clusters/{name}/access-entries, the EKS API facade endpoint
// matching the real `aws eks create-access-entry`.
func createAccessEntry(ctx context.Context, baseURL, principalARN, username string, groups []string) error {
	req := createAccessEntryRequest{PrincipalARN: principalARN, Username: username, KubernetesGroups: groups}

	if err := postFacadeJSON(ctx, baseURL+"/access-entries", req); err != nil {
		return fmt.Errorf("create access entry: %w", err)
	}

	return nil
}

// associateAccessPolicy grants principalARN policyARN via POST
// /clusters/{name}/access-entries/access-policies?principalArn=..., the EKS
// API facade endpoint matching the real `aws eks associate-access-policy`.
// A non-empty namespace scopes the grant to it; otherwise the grant is
// cluster-wide.
func associateAccessPolicy(ctx context.Context, baseURL, principalARN, policyARN, namespace string) error {
	scope := accessScopeRequest{Type: "cluster"}
	if namespace != "" {
		scope = accessScopeRequest{Type: "namespace", Namespaces: []string{namespace}}
	}

	req := associateAccessPolicyRequest{PolicyARN: policyARN, AccessScope: scope}

	endpoint := fmt.Sprintf("%s/access-entries/access-policies?principalArn=%s", baseURL, url.QueryEscape(principalARN))

	if err := postFacadeJSON(ctx, endpoint, req); err != nil {
		return fmt.Errorf("associate access policy: %w", err)
	}

	return nil
}

// postFacadeJSON POSTs body as JSON to endpoint (a fjord-agent EKS API
// facade URL), returning an error describing the facade's
// {"message": ...} error body on any non-200 response.
func postFacadeJSON(ctx context.Context, endpoint string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", endpoint, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return facadeError(resp)
	}

	return nil
}

// facadeError decodes resp's {"message": ...} error body into a descriptive
// error.
func facadeError(resp *http.Response) error {
	var errResp facadeErrorResponse

	_ = json.NewDecoder(resp.Body).Decode(&errResp)

	if errResp.Message != "" {
		return fmt.Errorf("facade request failed (%s): %s", resp.Status, errResp.Message)
	}

	return fmt.Errorf("facade request failed: %s", resp.Status)
}
