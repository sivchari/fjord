package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// StandardAccessPolicyClusterAdmin is the AWS-managed EKS access policy
	// ARN AssociateAccessPolicy maps to the built-in cluster-admin
	// ClusterRole.
	StandardAccessPolicyClusterAdmin = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
	// StandardAccessPolicyAdmin is the AWS-managed EKS access policy ARN
	// AssociateAccessPolicy maps to the built-in admin ClusterRole.
	StandardAccessPolicyAdmin = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy"
	// StandardAccessPolicyEdit is the AWS-managed EKS access policy ARN
	// AssociateAccessPolicy maps to the built-in edit ClusterRole.
	StandardAccessPolicyEdit = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy"
	// StandardAccessPolicyView is the AWS-managed EKS access policy ARN
	// AssociateAccessPolicy maps to the built-in view ClusterRole.
	StandardAccessPolicyView = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"

	accessScopeTypeCluster   = "cluster"
	accessScopeTypeNamespace = "namespace"

	eksErrorInvalidParameter = "InvalidParameterException"
	eksErrorResourceNotFound = "ResourceNotFoundException"
	eksErrorInvalidRequest   = "InvalidRequestException"
	eksErrorInternalServer   = "InternalServerException"

	// accessEntryBindingPrefix names every ClusterRoleBinding/RoleBinding
	// AssociateAccessPolicy materializes.
	accessEntryBindingPrefix = "fjord-access-entry-"

	// defaultPlatformVersion is the platformVersion DescribeCluster reports
	// when ClusterInfo has none registered (e.g. ClusterInfo predating the
	// PlatformVersion field).
	defaultPlatformVersion = "eks.1"

	// dummyClusterCreatedAt is the fixed createdAt DescribeCluster reports.
	// fjord does not track a real creation time, and this environment
	// disallows time.Now(), so every cluster reports the same placeholder.
	dummyClusterCreatedAt = "2024-01-01T00:00:00.000000+00:00"

	// dummyClusterRoleName is the IAM role name DescribeCluster's roleArn
	// reports. fjord has no real IAM role backing the cluster.
	dummyClusterRoleName = "fjord-cluster-role"
)

// createPodIdentityAssociationRequest is the request body of
// POST /clusters/{name}/pod-identity-associations.
type createPodIdentityAssociationRequest struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
	RoleARN        string `json:"roleArn"`
}

// createPodIdentityAssociationResponse is the response body of
// POST /clusters/{name}/pod-identity-associations.
type createPodIdentityAssociationResponse struct {
	Association podIdentityAssociationDetail `json:"association"`
}

// podIdentityAssociationDetail is the association shape
// createPodIdentityAssociationResponse embeds.
type podIdentityAssociationDetail struct {
	AssociationARN string `json:"associationArn"`
	AssociationID  string `json:"associationId"`
	ClusterName    string `json:"clusterName"`
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
	RoleARN        string `json:"roleArn"`
}

// handleCreatePodIdentityAssociation serves
// POST /clusters/{name}/pod-identity-associations, the EKS API facade
// endpoint `fjord create pod-identity-association`-equivalent tooling calls
// to register a ServiceAccount's IAM role. Registering an association here
// is what later lets handleAssumeRoleForPodIdentity resolve the
// ServiceAccount's pods to credentials for role, and what fjord's own
// Injector webhook checks to decide whether to inject Pod Identity's
// credential source into a pod.
func (s *Server) handleCreatePodIdentityAssociation(w http.ResponseWriter, r *http.Request) {
	var req createPodIdentityAssociationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.Namespace == "" || req.ServiceAccount == "" || req.RoleARN == "" {
		http.Error(w, "namespace, serviceAccount, and roleArn are required", http.StatusBadRequest)

		return
	}

	associationID, err := NewPodIdentityAssociationID()
	if err != nil {
		http.Error(w, "failed to generate association id", http.StatusInternalServerError)

		return
	}

	clusterName := r.PathValue("name")

	association := PodIdentityAssociation{
		Namespace:      req.Namespace,
		ServiceAccount: req.ServiceAccount,
		RoleARN:        req.RoleARN,
		AssociationID:  associationID,
	}

	if err := s.podIdentityStore.Put(r.Context(), association); err != nil {
		http.Error(w, "failed to create pod identity association: "+err.Error(), http.StatusInternalServerError)

		return
	}

	writeJSONResponse(w, createPodIdentityAssociationResponse{
		Association: podIdentityAssociationDetail{
			AssociationARN: PodIdentityAssociationARN(clusterName, associationID),
			AssociationID:  associationID,
			ClusterName:    clusterName,
			Namespace:      req.Namespace,
			ServiceAccount: req.ServiceAccount,
			RoleARN:        req.RoleARN,
		},
	})
}

// describeClusterResponse is the response body of GET /clusters/{name},
// matching the shape of the real `aws eks describe-cluster` response fields
// fjord update-kubeconfig needs.
type describeClusterResponse struct {
	Cluster describeClusterDetail `json:"cluster"`
}

// describeClusterDetail is the cluster shape describeClusterResponse embeds,
// matching the fields real `aws eks describe-cluster` returns that standard
// EKS tooling (aws eks / eksctl / terraform) expects to find.
type describeClusterDetail struct {
	Name                    string                                 `json:"name"`
	ARN                     string                                 `json:"arn"`
	Status                  string                                 `json:"status"`
	Endpoint                string                                 `json:"endpoint"`
	CertificateAuthority    describeClusterCertificateAuthority    `json:"certificateAuthority"`
	Version                 string                                 `json:"version"`
	PlatformVersion         string                                 `json:"platformVersion"`
	RoleARN                 string                                 `json:"roleArn"`
	CreatedAt               string                                 `json:"createdAt"`
	Identity                describeClusterIdentity                `json:"identity"`
	ResourcesVPCConfig      describeClusterResourcesVPCConfig      `json:"resourcesVpcConfig"`
	KubernetesNetworkConfig describeClusterKubernetesNetworkConfig `json:"kubernetesNetworkConfig"`
	Tags                    map[string]string                      `json:"tags"`
	Health                  describeClusterHealth                  `json:"health"`
	AccessConfig            describeClusterAccessConfig            `json:"accessConfig"`
}

// describeClusterCertificateAuthority is the certificateAuthority shape
// describeClusterDetail embeds.
type describeClusterCertificateAuthority struct {
	Data string `json:"data"`
}

// describeClusterIdentity is the identity shape describeClusterDetail
// embeds.
type describeClusterIdentity struct {
	OIDC describeClusterOIDC `json:"oidc"`
}

// describeClusterOIDC is the identity.oidc shape describeClusterIdentity
// embeds.
type describeClusterOIDC struct {
	Issuer string `json:"issuer"`
}

// describeClusterResourcesVPCConfig is the resourcesVpcConfig shape
// describeClusterDetail embeds.
type describeClusterResourcesVPCConfig struct {
	EndpointPublicAccess  bool `json:"endpointPublicAccess"`
	EndpointPrivateAccess bool `json:"endpointPrivateAccess"`
}

// describeClusterKubernetesNetworkConfig is the kubernetesNetworkConfig
// shape describeClusterDetail embeds.
type describeClusterKubernetesNetworkConfig struct {
	ServiceIPv4CIDR string `json:"serviceIpv4Cidr"`
	IPFamily        string `json:"ipFamily"`
}

// describeClusterHealth is the health shape describeClusterDetail and
// describeAddonDetail embed. fjord reports no issues.
type describeClusterHealth struct {
	Issues []string `json:"issues"`
}

// describeClusterAccessConfig is the accessConfig shape describeClusterDetail
// embeds.
type describeClusterAccessConfig struct {
	AuthenticationMode string `json:"authenticationMode"`
}

// handleDescribeCluster serves GET /clusters/{name}, the EKS API facade
// endpoint `fjord update-kubeconfig` and standard EKS tooling (aws eks /
// eksctl / terraform) call to resolve a cluster's details, matching the real
// `aws eks describe-cluster` response shape.
func (s *Server) handleDescribeCluster(w http.ResponseWriter, r *http.Request) {
	info, err := s.clusterInfoStore.Get(r.Context())
	if errors.Is(err, ErrClusterInfoNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "cluster info not registered")

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	writeJSONResponse(w, describeClusterResponse{Cluster: describeClusterDetailFor(r.PathValue("name"), info)})
}

// describeClusterDetailFor builds the describeClusterDetail for the cluster
// named clusterName, populating its EKS-version-dependent fields from info.
func describeClusterDetailFor(clusterName string, info *ClusterInfo) describeClusterDetail {
	platformVersion := info.PlatformVersion
	if platformVersion == "" {
		platformVersion = defaultPlatformVersion
	}

	return describeClusterDetail{
		Name:                 clusterName,
		ARN:                  clusterARN(clusterName),
		Status:               "ACTIVE",
		Endpoint:             info.Endpoint,
		CertificateAuthority: describeClusterCertificateAuthority{Data: info.CertificateAuthorityData},
		Version:              info.Version,
		PlatformVersion:      platformVersion,
		RoleARN:              fmt.Sprintf("arn:aws:iam::%s:role/%s", AccountID, dummyClusterRoleName),
		CreatedAt:            dummyClusterCreatedAt,
		Identity: describeClusterIdentity{
			OIDC: describeClusterOIDC{Issuer: fmt.Sprintf("https://oidc.eks.%s.amazonaws.com/id/FJORD%s", imdsRegion, clusterName)},
		},
		ResourcesVPCConfig: describeClusterResourcesVPCConfig{EndpointPublicAccess: true, EndpointPrivateAccess: false},
		KubernetesNetworkConfig: describeClusterKubernetesNetworkConfig{
			ServiceIPv4CIDR: "10.96.0.0/12",
			IPFamily:        "ipv4",
		},
		Tags:         map[string]string{},
		Health:       describeClusterHealth{Issues: []string{}},
		AccessConfig: describeClusterAccessConfig{AuthenticationMode: "API"},
	}
}

// listClustersResponse is the response body of GET /clusters.
type listClustersResponse struct {
	Clusters  []string `json:"clusters"`
	NextToken *string  `json:"nextToken"`
}

// handleListClusters serves GET /clusters, the EKS API facade endpoint
// standard EKS tooling (aws eks list-clusters) calls to discover clusters.
// fjord's facade is single-cluster: it reports the one cluster registered in
// clusterInfoStore, if any.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	info, err := s.clusterInfoStore.Get(r.Context())
	if errors.Is(err, ErrClusterInfoNotFound) {
		writeJSONResponse(w, listClustersResponse{Clusters: []string{}})

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	writeJSONResponse(w, listClustersResponse{Clusters: []string{info.Name}})
}

// clusterARN returns the ARN fjord assigns to the cluster named name.
func clusterARN(name string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:cluster/%s", imdsRegion, AccountID, name)
}

// createAccessEntryRequest is the request body of
// POST /clusters/{name}/access-entries.
type createAccessEntryRequest struct {
	PrincipalARN     string   `json:"principalArn"`
	Username         string   `json:"username,omitempty"`
	KubernetesGroups []string `json:"kubernetesGroups,omitempty"`
}

// accessEntryResponse is the response body of POST /clusters/{name}/access-entries.
type accessEntryResponse struct {
	AccessEntry accessEntryDetail `json:"accessEntry"`
}

// accessEntryDetail is the accessEntry shape accessEntryResponse embeds.
type accessEntryDetail struct {
	PrincipalARN     string   `json:"principalArn"`
	Username         string   `json:"username"`
	KubernetesGroups []string `json:"kubernetesGroups,omitempty"`
	Type             string   `json:"type"`
}

// toAccessEntryDetail builds entry's wire representation, resolving its
// effective (default-applied) username.
func toAccessEntryDetail(entry *AccessEntry) accessEntryDetail {
	return accessEntryDetail{
		PrincipalARN:     entry.PrincipalARN,
		Username:         entry.EffectiveUsername(),
		KubernetesGroups: entry.KubernetesGroups,
		Type:             entry.Type,
	}
}

// handleCreateAccessEntry serves POST /clusters/{name}/access-entries, the
// EKS API facade endpoint `fjord grant access-entry` calls to register a
// principal's Kubernetes identity, matching the real `aws eks
// create-access-entry` this whole flow emulates.
func (s *Server) handleCreateAccessEntry(w http.ResponseWriter, r *http.Request) {
	var req createAccessEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidRequest, "failed to parse request body")

		return
	}

	if req.PrincipalARN == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "principalArn is required")

		return
	}

	entry := AccessEntry{
		PrincipalARN:     req.PrincipalARN,
		Username:         req.Username,
		KubernetesGroups: req.KubernetesGroups,
		Type:             AccessEntryTypeStandard,
	}

	if err := s.accessEntryStore.Put(r.Context(), &entry); err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	writeJSONResponse(w, accessEntryResponse{AccessEntry: toAccessEntryDetail(&entry)})
}

// listAccessEntriesResponse is the response body of
// GET /clusters/{name}/access-entries.
type listAccessEntriesResponse struct {
	AccessEntries []string `json:"accessEntries"`
}

// handleListAccessEntries serves GET /clusters/{name}/access-entries,
// listing every registered principal ARN, matching the real `aws eks
// list-access-entries` response shape.
func (s *Server) handleListAccessEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := s.accessEntryStore.List(r.Context())
	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	arns := make([]string, 0, len(entries))
	for _, entry := range entries {
		arns = append(arns, entry.PrincipalARN)
	}

	writeJSONResponse(w, listAccessEntriesResponse{AccessEntries: arns})
}

// handleDeleteAccessEntry serves
// DELETE /clusters/{name}/access-entries?principalArn=..., removing the
// AccessEntry registered for the principalArn query parameter. It does not
// remove any ClusterRoleBinding/RoleBinding AssociateAccessPolicy created
// for it; callers should disassociate every policy first.
func (s *Server) handleDeleteAccessEntry(w http.ResponseWriter, r *http.Request) {
	principalARN := r.URL.Query().Get("principalArn")
	if principalARN == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "principalArn query parameter is required")

		return
	}

	err := s.accessEntryStore.Delete(r.Context(), principalARN)
	if errors.Is(err, ErrAccessEntryNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "access entry not found")

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	w.WriteHeader(http.StatusOK)
}

// accessScopeRequest is the accessScope shape associateAccessPolicyRequest
// embeds, matching the real EKS API's AccessScope.
type accessScopeRequest struct {
	Type       string   `json:"type"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// associateAccessPolicyRequest is the request body of
// POST /clusters/{name}/access-entries/access-policies?principalArn=....
type associateAccessPolicyRequest struct {
	PolicyARN   string             `json:"policyArn"`
	AccessScope accessScopeRequest `json:"accessScope"`
}

// handleAssociateAccessPolicy serves
// POST /clusters/{name}/access-entries/access-policies?principalArn=...,
// the EKS API facade endpoint `fjord grant access-entry --policy` calls.
// Unlike the real EKS API (which authorizes purely through its own control
// plane), fjord has no separate authorization layer: RBAC is real Kubernetes
// RBAC, so associating a standard access policy here immediately
// materializes a ClusterRoleBinding (cluster-scoped) or RoleBinding
// (namespace-scoped, one per requested namespace) binding the principal's
// synthetic access-entry group to the matching built-in ClusterRole
// (cluster-admin/admin/edit/view).
func (s *Server) handleAssociateAccessPolicy(w http.ResponseWriter, r *http.Request) {
	principalARN := r.URL.Query().Get("principalArn")
	if principalARN == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "principalArn query parameter is required")

		return
	}

	var req associateAccessPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidRequest, "failed to parse request body")

		return
	}

	clusterRoleName, scopeType, ok := s.validateAssociateAccessPolicyRequest(w, req)
	if !ok {
		return
	}

	entry, err := s.accessEntryStore.Get(r.Context(), principalARN)
	if errors.Is(err, ErrAccessEntryNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "access entry not found")

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	if err := materializeAccessPolicyRBAC(r.Context(), s.client, entry.PrincipalARN, clusterRoleName, scopeType, req.AccessScope.Namespaces); err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	entry.AssociatedPolicies = upsertAssociatedPolicy(entry.AssociatedPolicies, AssociatedAccessPolicy{
		PolicyARN:       req.PolicyARN,
		AccessScopeType: scopeType,
		Namespaces:      req.AccessScope.Namespaces,
	})

	if err := s.accessEntryStore.Put(r.Context(), entry); err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	w.WriteHeader(http.StatusOK)
}

// validateAssociateAccessPolicyRequest validates req, writing an error
// response and returning ok=false if it is invalid. On success it returns
// the ClusterRole to bind and the normalized (defaulted to "cluster")
// access scope type.
func (s *Server) validateAssociateAccessPolicyRequest(w http.ResponseWriter, req associateAccessPolicyRequest) (clusterRoleName, scopeType string, ok bool) {
	clusterRoleName, recognized := clusterRoleForPolicy(req.PolicyARN)
	if !recognized {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, fmt.Sprintf("unsupported policy arn %q", req.PolicyARN))

		return "", "", false
	}

	scopeType = req.AccessScope.Type
	if scopeType == "" {
		scopeType = accessScopeTypeCluster
	}

	if scopeType != accessScopeTypeCluster && scopeType != accessScopeTypeNamespace {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, fmt.Sprintf("unsupported accessScope.type %q", scopeType))

		return "", "", false
	}

	if scopeType == accessScopeTypeNamespace && len(req.AccessScope.Namespaces) == 0 {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "accessScope.namespaces is required when accessScope.type is \"namespace\"")

		return "", "", false
	}

	return clusterRoleName, scopeType, true
}

// listAssociatedAccessPoliciesResponse is the response body of
// GET /clusters/{name}/access-entries/access-policies?principalArn=....
type listAssociatedAccessPoliciesResponse struct {
	PrincipalARN             string                   `json:"principalArn"`
	AssociatedAccessPolicies []AssociatedAccessPolicy `json:"associatedAccessPolicies"`
}

// handleListAssociatedAccessPolicies serves
// GET /clusters/{name}/access-entries/access-policies?principalArn=....
func (s *Server) handleListAssociatedAccessPolicies(w http.ResponseWriter, r *http.Request) {
	principalARN := r.URL.Query().Get("principalArn")
	if principalARN == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "principalArn query parameter is required")

		return
	}

	entry, err := s.accessEntryStore.Get(r.Context(), principalARN)
	if errors.Is(err, ErrAccessEntryNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "access entry not found")

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	writeJSONResponse(w, listAssociatedAccessPoliciesResponse{
		PrincipalARN:             entry.PrincipalARN,
		AssociatedAccessPolicies: entry.AssociatedPolicies,
	})
}

// handleDisassociateAccessPolicy serves
// DELETE /clusters/{name}/access-entries/access-policies?principalArn=...&policyArn=...,
// removing the ClusterRoleBinding/RoleBinding(s) AssociateAccessPolicy
// created for the (principalArn, policyArn) pair.
func (s *Server) handleDisassociateAccessPolicy(w http.ResponseWriter, r *http.Request) {
	principalARN := r.URL.Query().Get("principalArn")
	policyARN := r.URL.Query().Get("policyArn")

	if principalARN == "" || policyARN == "" {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, "principalArn and policyArn query parameters are required")

		return
	}

	entry, err := s.accessEntryStore.Get(r.Context(), principalARN)
	if errors.Is(err, ErrAccessEntryNotFound) {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "access entry not found")

		return
	}

	if err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	clusterRoleName, ok := clusterRoleForPolicy(policyARN)
	if !ok {
		writeEKSAuthError(w, http.StatusBadRequest, eksErrorInvalidParameter, fmt.Sprintf("unsupported policy arn %q", policyARN))

		return
	}

	policy, ok := findAssociatedPolicy(entry.AssociatedPolicies, policyARN)
	if !ok {
		writeEKSAuthError(w, http.StatusNotFound, eksErrorResourceNotFound, "policy is not associated with this access entry")

		return
	}

	if err := removeAccessPolicyRBAC(r.Context(), s.client, entry.PrincipalARN, clusterRoleName, policy.AccessScopeType, policy.Namespaces); err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	entry.AssociatedPolicies = removeAssociatedPolicy(entry.AssociatedPolicies, policyARN)

	if err := s.accessEntryStore.Put(r.Context(), entry); err != nil {
		writeEKSAuthError(w, http.StatusInternalServerError, eksErrorInternalServer, err.Error())

		return
	}

	w.WriteHeader(http.StatusOK)
}

// clusterRoleForPolicy maps a standard EKS access policy ARN to the
// built-in ClusterRole AssociateAccessPolicy binds a principal's synthetic
// access-entry group to.
func clusterRoleForPolicy(policyARN string) (clusterRoleName string, ok bool) {
	switch path.Base(policyARN) {
	case "AmazonEKSClusterAdminPolicy":
		return "cluster-admin", true
	case "AmazonEKSAdminPolicy":
		return "admin", true
	case "AmazonEKSEditPolicy":
		return "edit", true
	case "AmazonEKSViewPolicy":
		return "view", true
	default:
		return "", false
	}
}

// upsertAssociatedPolicy replaces policies' entry for policy.PolicyARN if
// one already exists, or appends policy otherwise.
func upsertAssociatedPolicy(policies []AssociatedAccessPolicy, policy AssociatedAccessPolicy) []AssociatedAccessPolicy {
	for i := range policies {
		if policies[i].PolicyARN == policy.PolicyARN {
			policies[i] = policy

			return policies
		}
	}

	return append(policies, policy)
}

// findAssociatedPolicy returns policies' entry for policyARN, if any.
func findAssociatedPolicy(policies []AssociatedAccessPolicy, policyARN string) (AssociatedAccessPolicy, bool) {
	for _, p := range policies {
		if p.PolicyARN == policyARN {
			return p, true
		}
	}

	return AssociatedAccessPolicy{}, false
}

// removeAssociatedPolicy returns policies with policyARN's entry removed.
func removeAssociatedPolicy(policies []AssociatedAccessPolicy, policyARN string) []AssociatedAccessPolicy {
	filtered := make([]AssociatedAccessPolicy, 0, len(policies))

	for _, p := range policies {
		if p.PolicyARN != policyARN {
			filtered = append(filtered, p)
		}
	}

	return filtered
}

// accessEntryBindingName returns the deterministic
// ClusterRoleBinding/RoleBinding name AssociateAccessPolicy creates for the
// (principalARN, clusterRoleName) pair. It is stable so a later
// AssociateAccessPolicy call for the same pair is a no-op, and so
// DisassociateAccessPolicy can recompute it without any extra bookkeeping.
func accessEntryBindingName(principalARN, clusterRoleName string) string {
	return fmt.Sprintf("%s%s-%s", accessEntryBindingPrefix, principalHash(principalARN), clusterRoleName)
}

// materializeAccessPolicyRBAC creates the ClusterRoleBinding
// (accessScopeTypeCluster) or one RoleBinding per namespace
// (accessScopeTypeNamespace) binding principalARN's synthetic
// fjord:access-entry:<hash> group to clusterRoleName.
func materializeAccessPolicyRBAC(ctx context.Context, client kubernetes.Interface, principalARN, clusterRoleName, scopeType string, namespaces []string) error {
	group := principalGroup(principalARN)
	name := accessEntryBindingName(principalARN, clusterRoleName)

	if scopeType == accessScopeTypeCluster {
		return ensureAccessEntryClusterRoleBinding(ctx, client, name, clusterRoleName, group)
	}

	for _, namespace := range namespaces {
		if err := ensureAccessEntryRoleBinding(ctx, client, namespace, name, clusterRoleName, group); err != nil {
			return err
		}
	}

	return nil
}

// removeAccessPolicyRBAC deletes the ClusterRoleBinding/RoleBinding(s)
// materializeAccessPolicyRBAC created for the (principalARN, clusterRoleName)
// pair.
func removeAccessPolicyRBAC(ctx context.Context, client kubernetes.Interface, principalARN, clusterRoleName, scopeType string, namespaces []string) error {
	name := accessEntryBindingName(principalARN, clusterRoleName)

	if scopeType == accessScopeTypeCluster {
		return deleteAccessEntryClusterRoleBinding(ctx, client, name)
	}

	for _, namespace := range namespaces {
		if err := deleteAccessEntryRoleBinding(ctx, client, namespace, name); err != nil {
			return err
		}
	}

	return nil
}

// ensureAccessEntryClusterRoleBinding creates a ClusterRoleBinding named
// name binding group to clusterRoleName, if it does not already exist. Its
// RoleRef is immutable and its Subjects are fully determined by name (see
// accessEntryBindingName), so an already-existing binding never needs
// updating.
func ensureAccessEntryClusterRoleBinding(ctx context.Context, client kubernetes.Interface, name, clusterRoleName, group string) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRoleName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: group}},
	}

	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}

	return fmt.Errorf("create %q cluster role binding: %w", name, err)
}

// deleteAccessEntryClusterRoleBinding deletes the ClusterRoleBinding named
// name, treating it already being gone as success.
func deleteAccessEntryClusterRoleBinding(ctx context.Context, client kubernetes.Interface, name string) error {
	err := client.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}

	return fmt.Errorf("delete %q cluster role binding: %w", name, err)
}

// ensureAccessEntryRoleBinding creates a RoleBinding named name in namespace
// binding group to clusterRoleName (a ClusterRole, scoped to namespace by
// the RoleBinding), if it does not already exist.
func ensureAccessEntryRoleBinding(ctx context.Context, client kubernetes.Interface, namespace, name, clusterRoleName, group string) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRoleName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: group}},
	}

	_, err := client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}

	return fmt.Errorf("create %q role binding in %q: %w", name, namespace, err)
}

// deleteAccessEntryRoleBinding deletes the RoleBinding named name in
// namespace, treating it already being gone as success.
func deleteAccessEntryRoleBinding(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	err := client.RbacV1().RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}

	return fmt.Errorf("delete %q role binding in %q: %w", name, namespace, err)
}
