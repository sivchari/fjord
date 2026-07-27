package agent

import (
	"encoding/json"
	"net/http"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// facadeTestNamespace is the ServiceAccount namespace this file's test
// fixtures use, factored out because goconst flags "default" as a
// candidate constant once it repeats often enough in one file.
const facadeTestNamespace = "default"

func TestHandleCreatePodIdentityAssociation_Success(t *testing.T) {
	t.Parallel()

	store := NewInMemoryPodIdentityStore()
	server := newPodIdentityTestServer(fake.NewClientset(), store)

	rec := postJSON(t, server.Handler(), "/clusters/my-cluster/pod-identity-associations",
		createPodIdentityAssociationRequest{
			Namespace:      facadeTestNamespace,
			ServiceAccount: "app-sa",
			RoleARN:        "arn:aws:iam::000000000000:role/app-role",
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp createPodIdentityAssociationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, rec.Body.String())
	}

	assoc := resp.Association
	if assoc.Namespace != facadeTestNamespace || assoc.ServiceAccount != "app-sa" || assoc.RoleARN != "arn:aws:iam::000000000000:role/app-role" {
		t.Errorf("association = %+v, want namespace=%s serviceAccount=app-sa roleArn=arn:aws:iam::000000000000:role/app-role", assoc, facadeTestNamespace)
	}

	if assoc.ClusterName != "my-cluster" {
		t.Errorf("association.clusterName = %q, want %q", assoc.ClusterName, "my-cluster")
	}

	if assoc.AssociationID == "" {
		t.Error("association.associationId is empty")
	}

	wantARN := PodIdentityAssociationARN("my-cluster", assoc.AssociationID)
	if assoc.AssociationARN != wantARN {
		t.Errorf("association.associationArn = %q, want %q", assoc.AssociationARN, wantARN)
	}

	// The association must actually be registered, so a later
	// assume-role-for-pod-identity call can resolve it.
	stored, err := store.GetBySA(t.Context(), facadeTestNamespace, "app-sa")
	if err != nil {
		t.Fatalf("GetBySA: %v", err)
	}

	if stored.RoleARN != "arn:aws:iam::000000000000:role/app-role" || stored.AssociationID != assoc.AssociationID {
		t.Errorf("stored association = %+v, want role/associationId to match response", stored)
	}
}

func TestHandleCreatePodIdentityAssociation_MissingFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  createPodIdentityAssociationRequest
	}{
		{name: "missing namespace", req: createPodIdentityAssociationRequest{ServiceAccount: "app-sa", RoleARN: "arn:aws:iam::000000000000:role/app-role"}},
		{name: "missing service account", req: createPodIdentityAssociationRequest{Namespace: facadeTestNamespace, RoleARN: "arn:aws:iam::000000000000:role/app-role"}},
		{name: "missing role arn", req: createPodIdentityAssociationRequest{Namespace: facadeTestNamespace, ServiceAccount: "app-sa"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newPodIdentityTestServer(fake.NewClientset(), NewInMemoryPodIdentityStore())

			rec := postJSON(t, server.Handler(), "/clusters/my-cluster/pod-identity-associations", tt.req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

// TestPodIdentityFacadeThenAssumeRole is an end-to-end check within a single
// Server instance: an association created via the facade endpoint must
// immediately be resolvable by handleAssumeRoleForPodIdentity, matching how
// the two handlers share s.podIdentityStore.
func TestPodIdentityFacadeThenAssumeRole(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	server := newPodIdentityTestServer(client, NewInMemoryPodIdentityStore())
	handler := server.Handler()

	createRec := postJSON(t, handler, "/clusters/my-cluster/pod-identity-associations",
		createPodIdentityAssociationRequest{
			Namespace:      facadeTestNamespace,
			ServiceAccount: "app-sa",
			RoleARN:        "arn:aws:iam::000000000000:role/app-role",
		})
	if createRec.Code != http.StatusOK {
		t.Fatalf("create association status = %d, want %d; body: %s", createRec.Code, http.StatusOK, createRec.Body.String())
	}

	reactToTokenReview(client, &authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "system:serviceaccount:default:app-sa"},
	})

	assumeRec := postJSON(t, handler, "/clusters/my-cluster/assume-role-for-pod-identity",
		assumeRoleForPodIdentityRequest{Token: "any-token"})
	if assumeRec.Code != http.StatusOK {
		t.Fatalf("assume role status = %d, want %d; body: %s", assumeRec.Code, http.StatusOK, assumeRec.Body.String())
	}
}
