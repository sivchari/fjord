package agent

import (
	"encoding/json"
	"net/http"
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

	writeJSONResponse(w, http.StatusOK, createPodIdentityAssociationResponse{
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
