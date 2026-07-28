package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/sivchari/fjord/internal/agent"
)

// testKindContext is the kind-managed kubeconfig context name every test in
// this file seeds, standing in for the one `fjord create cluster` leaves
// behind.
const testKindContext = "kind-fjord"

func TestExecAuthInfo(t *testing.T) {
	t.Parallel()

	opts := &updateKubeconfigOptions{
		principalRegistryOptions: principalRegistryOptions{clusterName: "fjord"},
		principal:                "alice",
		hostPort:                 48080,
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

	if env["AWS_ENDPOINT_URL_STS"] != "http://localhost:48080" {
		t.Errorf("AWS_ENDPOINT_URL_STS = %q, want %q", env["AWS_ENDPOINT_URL_STS"], "http://localhost:48080")
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
	seed.CurrentContext = testKindContext

	if err := clientcmd.WriteToFile(*seed, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	config, err := loadOrNewKubeconfig(path)
	if err != nil {
		t.Fatalf("loadOrNewKubeconfig() error = %v", err)
	}

	if config.CurrentContext != testKindContext {
		t.Errorf("CurrentContext = %q, want %q", config.CurrentContext, testKindContext)
	}
}

func TestLoadKindClusterConfig_MissingContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	empty := clientcmdapi.NewConfig()
	if err := clientcmd.WriteToFile(*empty, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	if _, err := loadKindClusterConfig(testKindContext); err == nil {
		t.Fatal("loadKindClusterConfig() error = nil, want an error")
	}
}

// TestWriteKubeconfigEntry exercises loadKindClusterConfig and
// writeKubeconfigEntry together against a seeded kubeconfig standing in for
// the one kind writes on `fjord create cluster`, verifying the new
// principal context is added (and made current) without disturbing the
// existing kind-fjord entry.
func TestWriteKubeconfigEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	existing := clientcmdapi.NewConfig()
	existing.Clusters[testKindContext] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: []byte("fake-ca"),
	}
	existing.Contexts[testKindContext] = &clientcmdapi.Context{Cluster: testKindContext, AuthInfo: testKindContext}
	existing.AuthInfos[testKindContext] = &clientcmdapi.AuthInfo{Token: "fake-token"}
	existing.CurrentContext = testKindContext

	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	sourceCluster, err := loadKindClusterConfig(testKindContext)
	if err != nil {
		t.Fatalf("loadKindClusterConfig() error = %v", err)
	}

	opts := &updateKubeconfigOptions{
		principalRegistryOptions: principalRegistryOptions{clusterName: "fjord"},
		principal:                "alice",
		hostPort:                 48080,
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
// and made current without removing the pre-existing kind-fjord entry.
func assertWrittenKubeconfig(t *testing.T, path, wantContext string) {
	t.Helper()

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if updated.CurrentContext != wantContext {
		t.Errorf("CurrentContext = %q, want %q", updated.CurrentContext, wantContext)
	}

	if _, ok := updated.Contexts[testKindContext]; !ok {
		t.Errorf("pre-existing context %q was removed", testKindContext)
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
