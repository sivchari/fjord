package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/cluster"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// testClusterContext is the kubeconfig context name every test in this file
// seeds, standing in for the one `fjord create cluster` leaves behind (rask
// names it exactly the cluster name).
const testClusterContext = "fjord"

// testClusterServer is the apiserver endpoint the fixture kubeconfigs point
// at; rask always binds the control plane to loopback.
const testClusterServer = "https://127.0.0.1:6443"

// otherClusterContext is an unrelated pre-existing kubeconfig entry the
// merge/remove tests seed alongside testClusterContext, to verify it
// survives untouched.
const otherClusterContext = "other-cluster"

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

	wantEndpoint := fmt.Sprintf("http://%s:%d", cluster.AgentLoopbackHost, cluster.AgentNodePort)
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
		Server:                   testClusterServer,
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

	if newCluster.Server != testClusterServer {
		t.Errorf("new cluster Server = %q, want %q", newCluster.Server, testClusterServer)
	}

	newAuthInfo, ok := updated.AuthInfos[wantContext]
	if !ok || newAuthInfo.Exec == nil {
		t.Fatalf("new auth info %q missing an exec config", wantContext)
	}
}

// fakeKubeconfigProvider is a clusterprovider.Provider stub that returns a
// fixed KubeConfig result, for exercising mergeClusterKubeconfig without a
// real rask provider. Every other method is unused by these tests.
type fakeKubeconfigProvider struct {
	kubeconfig string
	err        error
}

func (f *fakeKubeconfigProvider) CreateCluster(context.Context, string, clusterprovider.CreateOptions) error {
	return nil
}

func (f *fakeKubeconfigProvider) DeleteCluster(context.Context, string, string) error {
	return nil
}

func (f *fakeKubeconfigProvider) ListClusters() ([]string, error) {
	return nil, nil
}

func (f *fakeKubeconfigProvider) KubeConfig(string) (string, error) {
	return f.kubeconfig, f.err
}

func (f *fakeKubeconfigProvider) LoadDockerImage(context.Context, string, string) error {
	return nil
}

// raskStyleKubeconfig builds kubeconfig content shaped like the one rask
// writes for the testClusterContext cluster: the Cluster and Context
// entries are named exactly testClusterContext, but the AuthInfo entry is
// always named "admin" (see internal/rask/provider.go's KubeConfig doc and
// rask's own internal/bootstrap/pki.go, which issues the admin credential
// under that fixed name regardless of cluster name).
func raskStyleKubeconfig(t *testing.T) string {
	t.Helper()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[testClusterContext] = &clientcmdapi.Cluster{Server: testClusterServer, CertificateAuthorityData: []byte("fake-ca")}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("fake-cert"), ClientKeyData: []byte("fake-key")}
	cfg.Contexts[testClusterContext] = &clientcmdapi.Context{Cluster: testClusterContext, AuthInfo: "admin"}
	cfg.CurrentContext = testClusterContext

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("build source kubeconfig: %v", err)
	}

	return string(data)
}

// assertMergedClusterKubeconfig reloads path and verifies name's Cluster,
// AuthInfo, and Context entries were merged in under name (with AuthInfo
// renamed from rask's "admin" to name) and made current, matching
// wantServer.
func assertMergedClusterKubeconfig(t *testing.T, path, name, wantServer string) {
	t.Helper()

	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if config.CurrentContext != name {
		t.Errorf("CurrentContext = %q, want %q", config.CurrentContext, name)
	}

	mergedCluster, ok := config.Clusters[name]
	if !ok {
		t.Fatalf("cluster entry %q not found", name)
	}

	if mergedCluster.Server != wantServer {
		t.Errorf("cluster Server = %q, want %q", mergedCluster.Server, wantServer)
	}

	if _, ok := config.AuthInfos[name]; !ok {
		t.Errorf("authinfo entry %q not found (rask's \"admin\" entry was not renamed)", name)
	}

	mergedContext, ok := config.Contexts[name]
	if !ok {
		t.Fatalf("context entry %q not found", name)
	}

	if mergedContext.Cluster != name || mergedContext.AuthInfo != name {
		t.Errorf("context %q = %+v, want Cluster and AuthInfo both %q", name, mergedContext, name)
	}
}

func TestMergeClusterKubeconfig_EmptyDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	provider := &fakeKubeconfigProvider{kubeconfig: raskStyleKubeconfig(t)}

	gotPath, err := mergeClusterKubeconfig(provider, testClusterContext)
	if err != nil {
		t.Fatalf("mergeClusterKubeconfig() error = %v", err)
	}

	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}

	assertMergedClusterKubeconfig(t, path, testClusterContext, testClusterServer)
}

func TestMergeClusterKubeconfig_PreservesUnrelatedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	existing := clientcmdapi.NewConfig()
	existing.Clusters[otherClusterContext] = &clientcmdapi.Cluster{Server: "https://example.com:6443"}
	existing.AuthInfos[otherClusterContext] = &clientcmdapi.AuthInfo{Token: "other-token"}
	existing.Contexts[otherClusterContext] = &clientcmdapi.Context{Cluster: otherClusterContext, AuthInfo: otherClusterContext}
	existing.CurrentContext = otherClusterContext

	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	provider := &fakeKubeconfigProvider{kubeconfig: raskStyleKubeconfig(t)}

	if _, err := mergeClusterKubeconfig(provider, testClusterContext); err != nil {
		t.Fatalf("mergeClusterKubeconfig() error = %v", err)
	}

	assertMergedClusterKubeconfig(t, path, testClusterContext, testClusterServer)

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if _, ok := updated.Clusters[otherClusterContext]; !ok {
		t.Errorf("pre-existing cluster %q was removed", otherClusterContext)
	}

	if _, ok := updated.Contexts[otherClusterContext]; !ok {
		t.Errorf("pre-existing context %q was removed", otherClusterContext)
	}
}

func TestMergeClusterKubeconfig_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	provider := &fakeKubeconfigProvider{kubeconfig: raskStyleKubeconfig(t)}

	if _, err := mergeClusterKubeconfig(provider, testClusterContext); err != nil {
		t.Fatalf("mergeClusterKubeconfig() first call error = %v", err)
	}

	if _, err := mergeClusterKubeconfig(provider, testClusterContext); err != nil {
		t.Fatalf("mergeClusterKubeconfig() second call error = %v", err)
	}

	assertMergedClusterKubeconfig(t, path, testClusterContext, testClusterServer)

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if len(updated.Clusters) != 1 || len(updated.AuthInfos) != 1 || len(updated.Contexts) != 1 {
		t.Errorf("re-merging created duplicate entries: clusters=%v authinfos=%v contexts=%v",
			updated.Clusters, updated.AuthInfos, updated.Contexts)
	}
}

func TestRemoveClusterKubeconfig_RemovesOwnEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	existing := clientcmdapi.NewConfig()
	existing.Clusters[otherClusterContext] = &clientcmdapi.Cluster{Server: "https://example.com:6443"}
	existing.AuthInfos[otherClusterContext] = &clientcmdapi.AuthInfo{Token: "other-token"}
	existing.Contexts[otherClusterContext] = &clientcmdapi.Context{Cluster: otherClusterContext, AuthInfo: otherClusterContext}
	existing.CurrentContext = otherClusterContext

	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	provider := &fakeKubeconfigProvider{kubeconfig: raskStyleKubeconfig(t)}

	if _, err := mergeClusterKubeconfig(provider, testClusterContext); err != nil {
		t.Fatalf("mergeClusterKubeconfig() error = %v", err)
	}

	if err := removeClusterKubeconfig(testClusterContext); err != nil {
		t.Fatalf("removeClusterKubeconfig() error = %v", err)
	}

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if _, ok := updated.Clusters[testClusterContext]; ok {
		t.Errorf("cluster entry %q was not removed", testClusterContext)
	}

	if _, ok := updated.AuthInfos[testClusterContext]; ok {
		t.Errorf("authinfo entry %q was not removed", testClusterContext)
	}

	if _, ok := updated.Contexts[testClusterContext]; ok {
		t.Errorf("context entry %q was not removed", testClusterContext)
	}

	if updated.CurrentContext != "" {
		t.Errorf("CurrentContext = %q, want empty (it pointed at the deleted cluster)", updated.CurrentContext)
	}

	if _, ok := updated.Clusters[otherClusterContext]; !ok {
		t.Errorf("pre-existing cluster %q was removed", otherClusterContext)
	}
}

func TestRemoveClusterKubeconfig_NoOpWhenFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	if err := removeClusterKubeconfig(testClusterContext); err != nil {
		t.Fatalf("removeClusterKubeconfig() error = %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("removeClusterKubeconfig() created %q, want it to remain absent", path)
	}
}

func TestRemoveClusterKubeconfig_NoOpWhenClusterNotMerged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("KUBECONFIG", path)

	existing := clientcmdapi.NewConfig()
	existing.Clusters[otherClusterContext] = &clientcmdapi.Cluster{Server: "https://example.com:6443"}
	existing.Contexts[otherClusterContext] = &clientcmdapi.Context{Cluster: otherClusterContext, AuthInfo: otherClusterContext}
	existing.AuthInfos[otherClusterContext] = &clientcmdapi.AuthInfo{Token: "other-token"}
	existing.CurrentContext = otherClusterContext

	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatalf("seed kubeconfig: %v", err)
	}

	if err := removeClusterKubeconfig(testClusterContext); err != nil {
		t.Fatalf("removeClusterKubeconfig() error = %v", err)
	}

	updated, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("reload kubeconfig: %v", err)
	}

	if updated.CurrentContext != otherClusterContext {
		t.Errorf("CurrentContext = %q, want unchanged %q", updated.CurrentContext, otherClusterContext)
	}

	if _, ok := updated.Clusters[otherClusterContext]; !ok {
		t.Errorf("pre-existing cluster %q was removed", otherClusterContext)
	}
}
