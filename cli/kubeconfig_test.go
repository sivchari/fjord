package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/cluster"
)

// testClusterContext is the kubeconfig context name every test in this file
// seeds, standing in for the one `fjord create cluster` leaves behind (rask
// names it exactly the cluster name).
const testClusterContext = "fjord"

func TestExecAuthInfo(t *testing.T) {
	t.Parallel()

	opts := &updateKubeconfigOptions{
		principalRegistryOptions: principalRegistryOptions{clusterName: "fjord"},
		principal:                "alice",
	}
	principal := &agent.Principal{AccessKeyID: "AKIAEXAMPLE", ARN: agent.PrincipalARN("alice"), Name: "alice"}

	authInfo := execAuthInfo(opts, principal)
	if authInfo.Exec == nil {
		t.Fatalf("Exec is nil")
	}

	wantArgs := []string{"--region", "us-east-1", "eks", "get-token", "--cluster-name", "fjord"}
	if !slices.Equal(authInfo.Exec.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", authInfo.Exec.Args, wantArgs)
	}

	env := execEnvMap(authInfo.Exec.Env)

	wantEndpoint := fmt.Sprintf("http://localhost:%d", cluster.AgentNodePort)
	if env["AWS_ENDPOINT_URL_STS"] != wantEndpoint {
		t.Errorf("AWS_ENDPOINT_URL_STS = %q, want %q", env["AWS_ENDPOINT_URL_STS"], wantEndpoint)
	}

	if env["AWS_ACCESS_KEY_ID"] != "AKIAEXAMPLE" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want %q", env["AWS_ACCESS_KEY_ID"], "AKIAEXAMPLE")
	}

	if env["AWS_SECRET_ACCESS_KEY"] == "" {
		t.Errorf("AWS_SECRET_ACCESS_KEY is empty")
	}

	if authInfo.Exec.InteractiveMode != clientcmdapi.NeverExecInteractiveMode {
		t.Errorf("InteractiveMode = %q, want %q", authInfo.Exec.InteractiveMode, clientcmdapi.NeverExecInteractiveMode)
	}
}

// execEnvMap indexes vars by name for assertions.
func execEnvMap(vars []clientcmdapi.ExecEnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}

	return m
}

func TestLoadOrNewKubeconfig_MissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist", "config")

	config, err := loadOrNewKubeconfig(path)
	if err != nil {
		t.Fatalf("loadOrNewKubeconfig() error = %v", err)
	}

	if config.Clusters == nil || config.AuthInfos == nil || config.Contexts == nil {
		t.Errorf("loadOrNewKubeconfig() returned nil maps: %+v", config)
	}
}

func TestLoadOrNewKubeconfig_ExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config")

	seed := clientcmdapi.NewConfig()
	seed.CurrentContext = testClusterContext

	if err := clientcmd.WriteToFile(*seed, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	config, err := loadOrNewKubeconfig(path)
	if err != nil {
		t.Fatalf("loadOrNewKubeconfig() error = %v", err)
	}

	if config.CurrentContext != testClusterContext {
		t.Errorf("CurrentContext = %q, want %q", config.CurrentContext, testClusterContext)
	}
}

func TestLoadClusterConfig_MissingContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	empty := clientcmdapi.NewConfig()
	if err := clientcmd.WriteToFile(*empty, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	if _, err := loadClusterConfig(testClusterContext); err == nil {
		t.Fatal("loadClusterConfig() error = nil, want an error")
	}
}

// TestWriteKubeconfigEntry exercises loadClusterConfig and
// writeKubeconfigEntry together against a seeded kubeconfig standing in for
// the one rask writes on `fjord create cluster`, verifying the new
// principal context is added (and made current) without disturbing the
// existing entry.
func TestWriteKubeconfigEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	existing := clientcmdapi.NewConfig()
	existing.Clusters[testClusterContext] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: []byte("fake-ca"),
	}
	existing.Contexts[testClusterContext] = &clientcmdapi.Context{Cluster: testClusterContext, AuthInfo: testClusterContext}
	existing.AuthInfos[testClusterContext] = &clientcmdapi.AuthInfo{Token: "fake-token"}
	existing.CurrentContext = testClusterContext

	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	sourceCluster, err := loadClusterConfig(testClusterContext)
	if err != nil {
		t.Fatalf("loadClusterConfig() error = %v", err)
	}

	opts := &updateKubeconfigOptions{
		principalRegistryOptions: principalRegistryOptions{clusterName: "fjord"},
		principal:                "alice",
	}
	principal := &agent.Principal{AccessKeyID: "AKIAEXAMPLE", ARN: agent.PrincipalARN("alice"), Name: "alice"}

	gotPath, contextName, err := writeKubeconfigEntry(opts, principal, sourceCluster)
	if err != nil {
		t.Fatalf("writeKubeconfigEntry() error = %v", err)
	}

	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}

	wantContext := "fjord-alice@fjord"
	if contextName != wantContext {
		t.Errorf("contextName = %q, want %q", contextName, wantContext)
	}

	assertWrittenKubeconfig(t, path, wantContext)
}

// assertWrittenKubeconfig reloads path and verifies wantContext was added
// and made current without removing the pre-existing entry.
func assertWrittenKubeconfig(t *testing.T, path, wantContext string) {
	t.Helper()

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if updated.CurrentContext != wantContext {
		t.Errorf("CurrentContext = %q, want %q", updated.CurrentContext, wantContext)
	}

	if _, ok := updated.Contexts[testClusterContext]; !ok {
		t.Errorf("pre-existing context %q was removed", testClusterContext)
	}

	newCluster, ok := updated.Clusters[wantContext]
	if !ok {
		t.Fatalf("new cluster entry %q not found", wantContext)
	}

	if newCluster.Server != "https://127.0.0.1:6443" {
		t.Errorf("new cluster Server = %q, want %q", newCluster.Server, "https://127.0.0.1:6443")
	}

	newAuthInfo, ok := updated.AuthInfos[wantContext]
	if !ok || newAuthInfo.Exec == nil {
		t.Fatalf("new auth info %q missing an exec config", wantContext)
	}
}
