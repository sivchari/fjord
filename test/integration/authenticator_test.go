//go:build integration

package integration

import (
	"os"
	"os/exec"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// authenticatorClusterName is the cluster name used by the
	// authenticator e2e test, kept distinct from every other e2e test's
	// cluster name so none of them collide if run concurrently.
	authenticatorClusterName = clusterName + "-authn"

	// authenticatorAgentHostPort is the host port fjord-agent's fake STS
	// API (and, on the same mux, its EKS API facade) is published on for
	// this test.
	authenticatorAgentHostPort = "48083"

	authenticatorPrincipalName = "authn-e2e-viewer"

	// authenticatorBogusAccessKeyID is shaped like a real fjord access key
	// id (see agent.GenerateAccessKeyID) but was never registered by
	// `fjord create principal`, exercising the "unregistered principal"
	// case: the authenticator's principal store lookup itself fails,
	// rather than just its access-entry lookup.
	authenticatorBogusAccessKeyID = "AKIAUNREGISTEREDXXXXXXX"
)

// TestAuthenticatorAccess is e2e⑤ from plan-v1.md Phase 7: it creates a
// cluster with fjord-agent auth enabled, registers a principal and grants
// it the View access policy via `fjord grant access-entry`, and points
// kubectl at it via `fjord update-kubeconfig`. A successful run proves the
// full authentication chain from plan-v1.md's Phase 7 design:
//
//  1. kube-apiserver's authentication-token-webhook-config-file points at
//     fjord-authenticator's static pod (internal/authn.Stage), reachable at
//     127.0.0.1 because both run hostNetwork in the control-plane node's
//     network namespace.
//  2. fjord-authenticator (internal/agent.Authenticator) resolves the
//     token's access key id against the principal registry and its access
//     entry, returning the principal's Kubernetes identity.
//  3. AssociateAccessPolicy (internal/agent/facade.go) materialized a
//     ClusterRoleBinding binding the principal's synthetic access-entry
//     group to the built-in "view" ClusterRole.
//
// It verifies: a read (list pods) succeeds, a write (create a configmap)
// is forbidden, and a request carrying an access key id that was never
// registered is rejected as unauthenticated.
//
// This builds a full cluster (~tens of minutes) and requires the `aws` CLI
// on the host (kubectl's exec credential plugin shells out to it), so it is
// not run as part of the default suite. Run it explicitly:
//
//	go test -C test -tags=integration -run TestAuthenticatorAccess -v ./integration/...
func TestAuthenticatorAccess(t *testing.T) {
	t.Skip("run explicitly: builds a full cluster with fjord-agent auth enabled and requires the aws CLI on the host")

	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", authenticatorClusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	createAuthenticatorCluster(t, bin)
	createAuthenticatorPrincipal(t, bin)
	grantAuthenticatorViewAccess(t, bin)
	contextName := updateAuthenticatorKubeconfig(t, bin)

	t.Cleanup(func() { removeKubeconfigContext(t, contextName) })

	config := authenticatorClientConfig(t, contextName)
	client := authenticatorClient(t, config)

	assertViewerCanList(t, client)
	assertViewerCannotCreate(t, client)
	assertUnregisteredPrincipalRejected(t, config)
}

// createAuthenticatorCluster runs `fjord create cluster` for
// authenticatorClusterName, with auth (and therefore the authenticator)
// enabled.
func createAuthenticatorCluster(t *testing.T, bin string) {
	t.Helper()

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", authenticatorClusterName,
		"--build-local",
		"--agent-host-port", authenticatorAgentHostPort,
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	if err := create.Run(); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
}

// createAuthenticatorPrincipal registers authenticatorPrincipalName via
// `fjord create principal`.
func createAuthenticatorPrincipal(t *testing.T, bin string) {
	t.Helper()

	cmd := exec.Command(bin, "create", "principal", authenticatorPrincipalName, "--name", authenticatorClusterName)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create principal: %v\n%s", err, out)
	}
}

// grantAuthenticatorViewAccess grants authenticatorPrincipalName the View
// access policy via `fjord grant access-entry`.
func grantAuthenticatorViewAccess(t *testing.T, bin string) {
	t.Helper()

	cmd := exec.Command(bin, "grant", "access-entry",
		"--name", authenticatorClusterName,
		"--principal", authenticatorPrincipalName,
		"--policy", "View",
		"--agent-host-port", authenticatorAgentHostPort,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grant access-entry: %v\n%s", err, out)
	}
}

// updateAuthenticatorKubeconfig runs `fjord update-kubeconfig` for
// authenticatorPrincipalName and returns the context name it added
// ("fjord-<principal>@<cluster>").
func updateAuthenticatorKubeconfig(t *testing.T, bin string) string {
	t.Helper()

	cmd := exec.Command(bin, "update-kubeconfig",
		"--name", authenticatorClusterName,
		"--principal", authenticatorPrincipalName,
		"--agent-host-port", authenticatorAgentHostPort,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-kubeconfig: %v\n%s", err, out)
	}

	return "fjord-" + authenticatorPrincipalName + "@" + authenticatorClusterName
}

// authenticatorClientConfig builds a *rest.Config for contextName from the
// default kubeconfig `fjord update-kubeconfig` wrote it to.
func authenticatorClientConfig(t *testing.T, contextName string) *rest.Config {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	return config
}

// authenticatorClient builds a Kubernetes client from config.
func authenticatorClient(t *testing.T, config *rest.Config) kubernetes.Interface {
	t.Helper()

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}

	return client
}

// assertViewerCanList verifies the View policy allows listing pods.
func assertViewerCanList(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	if _, err := client.CoreV1().Pods(metav1.NamespaceDefault).List(t.Context(), metav1.ListOptions{}); err != nil {
		t.Errorf("list pods with View access = %v, want success", err)
	}
}

// assertViewerCannotCreate verifies the View policy forbids creating
// resources.
func assertViewerCannotCreate(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "authn-e2e-should-be-forbidden"}}

	_, err := client.CoreV1().ConfigMaps(metav1.NamespaceDefault).Create(t.Context(), cm, metav1.CreateOptions{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("create configmap with View access, error = %v, want a Forbidden error", err)
	}
}

// assertUnregisteredPrincipalRejected verifies a request signed with an
// access key id that was never registered in the principal store is
// rejected as unauthenticated, not merely unauthorized: it rebuilds
// baseConfig's exec credential env with authenticatorBogusAccessKeyID
// instead of calling the CLI again.
func assertUnregisteredPrincipalRejected(t *testing.T, baseConfig *rest.Config) {
	t.Helper()

	bogusConfig := bogusPrincipalConfig(baseConfig)

	client, err := kubernetes.NewForConfig(bogusConfig)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}

	_, err = client.CoreV1().Pods(metav1.NamespaceDefault).List(t.Context(), metav1.ListOptions{})
	if !apierrors.IsUnauthorized(err) {
		t.Errorf("list pods with an unregistered principal, error = %v, want an Unauthorized error", err)
	}
}

// bogusPrincipalConfig copies baseConfig, replacing its exec credential's
// AWS_ACCESS_KEY_ID with authenticatorBogusAccessKeyID.
func bogusPrincipalConfig(baseConfig *rest.Config) *rest.Config {
	bogus := *baseConfig
	execCfg := *baseConfig.ExecProvider
	execCfg.Env = make([]clientcmdapi.ExecEnvVar, len(baseConfig.ExecProvider.Env))

	copy(execCfg.Env, baseConfig.ExecProvider.Env)

	for i, e := range execCfg.Env {
		if e.Name == "AWS_ACCESS_KEY_ID" {
			execCfg.Env[i].Value = authenticatorBogusAccessKeyID
		}
	}

	bogus.ExecProvider = &execCfg

	return &bogus
}

// removeKubeconfigContext removes contextName (and its Cluster/AuthInfo
// entries) from the default kubeconfig `fjord update-kubeconfig` wrote to,
// so re-running this test does not accumulate stale entries.
func removeKubeconfigContext(t *testing.T, contextName string) {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	path := rules.GetDefaultFilename()

	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Logf("load kubeconfig for cleanup: %v", err)

		return
	}

	delete(config.Contexts, contextName)
	delete(config.Clusters, contextName)
	delete(config.AuthInfos, contextName)

	if config.CurrentContext == contextName {
		config.CurrentContext = ""
	}

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Logf("write kubeconfig for cleanup: %v", err)
	}
}
