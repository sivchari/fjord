//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// imdsClusterName is the cluster name used by the IMDS e2e test, kept
	// distinct from every other e2e test's cluster name so none of them
	// collide if run concurrently.
	imdsClusterName = clusterName + "-imds"

	// imdsAgentHostPort is the host port fjord-agent's fake STS API is
	// published on for this test.
	imdsAgentHostPort = "48081"

	imdsNamespace   = "imds-e2e"
	imdsPodName     = "imds-e2e-pod"
	imdsContainer   = "curl"
	imdsNodeRole    = "fjord-node-role"
	imdsWaitTimeout = 3 * time.Minute

	// curlImage bundles curl and a shell, used to drive the IMDSv2 flow
	// from inside a pod that carries no ServiceAccount annotation.
	curlImage = "curlimages/curl:8.10.1"
)

// imdsCredentialsResponse is the subset of the IMDS security-credentials
// response this test needs.
type imdsCredentialsResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
}

// TestIMDSGetCallerIdentity is e2e③ from plan-v1.md Phase 5: it creates a
// cluster with fjord-agent auth (and therefore fjord-imds) enabled, and
// verifies that a plain pod carrying no eks.amazonaws.com/role-arn
// annotation can still complete the IMDSv2 flow against
// 169.254.169.254 (PUT a token, list the node role, fetch its temporary
// credentials) and that those credentials resolve to the node role's
// assumed-role ARN via `aws sts get-caller-identity`. A successful run
// proves the full IMDS chain from plan-v1.md's Phase 5 design:
//
//  1. fjord-imds (internal/cluster.EnsureIMDS's DaemonSet) added
//     169.254.169.254 to the node's loopback interface and served IMDSv2
//     there.
//  2. internal/agent.IMDS issued node-role credentials and registered
//     their access key in the fjord-principals Secret under the node
//     role's assumed-role ARN.
//  3. fjord-agent's fake STS (internal/agent/sts.go) resolved
//     GetCallerIdentity for that access key to the same assumed-role ARN,
//     proving IMDS and the fake STS share one principal registry across
//     their separate processes.
//
// This builds a full cluster (~tens of minutes) and requires the `aws` CLI
// and pulling the curl image, so it is not run as part of the default
// suite. Run it explicitly:
//
//	go test -C test -tags=integration -run TestIMDSGetCallerIdentity -v ./integration/...
func TestIMDSGetCallerIdentity(t *testing.T) {
	t.Skip("run explicitly: builds a full cluster with fjord-agent auth (IMDS) enabled")

	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", imdsClusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", imdsClusterName,
		"--build-local",
		"--agent-host-port", imdsAgentHostPort,
		"--node-role-name", imdsNodeRole,
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	if err := create.Run(); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	config, client := imdsClientConfig(t)

	pod := createIMDSPod(t, client)
	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	token := curlInPod(t, config, client, pod,
		"-s", "-X", "PUT", "http://169.254.169.254/latest/api/token",
		"-H", "X-aws-ec2-metadata-token-ttl-seconds: 21600")

	roleList := curlInPod(t, config, client, pod,
		"-s", "-H", "X-aws-ec2-metadata-token: "+token,
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/")

	role := strings.TrimSpace(roleList)
	if role != imdsNodeRole {
		t.Fatalf("imds role list = %q, want %q", role, imdsNodeRole)
	}

	credsBody := curlInPod(t, config, client, pod,
		"-s", "-H", "X-aws-ec2-metadata-token: "+token,
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/"+role)

	var creds imdsCredentialsResponse
	if err := json.Unmarshal([]byte(credsBody), &creds); err != nil {
		t.Fatalf("unmarshal imds credentials: %v\n%s", err, credsBody)
	}

	wantARNContains := "assumed-role/" + imdsNodeRole + "/"
	assertIMDSCredentialsCallerIdentity(t, creds, wantARNContains)
}

// imdsClientConfig builds a *rest.Config and Kubernetes client from the
// kubeconfig context kind wrote for the IMDS e2e cluster.
func imdsClientConfig(t *testing.T) (*rest.Config, kubernetes.Interface) {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + imdsClusterName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}

	return config, client
}

// createIMDSPod creates a pod running curlImage with no ServiceAccount
// annotation, so it is never mutated by pod-identity-webhook or fjord's own
// injector, and only IMDS can supply it credentials.
func createIMDSPod(t *testing.T, client kubernetes.Interface) *corev1.Pod {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: imdsNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: imdsPodName, Namespace: imdsNamespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: imdsContainer, Image: curlImage, Command: []string{"sleep", "3600"}},
			},
		},
	}

	created, err := client.CoreV1().Pods(imdsNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	return created
}

// curlInPod runs curl with args inside pod's imdsContainer and returns its
// stdout.
func curlInPod(t *testing.T, config *rest.Config, client kubernetes.Interface, pod *corev1.Pod, args ...string) string {
	t.Helper()

	return execInPod(t, config, client, pod.Namespace, pod.Name, append([]string{"curl"}, args...))
}

// assertIMDSCredentialsCallerIdentity runs `aws sts get-caller-identity`
// against fjord-agent's fake STS endpoint (published on imdsAgentHostPort
// by its NodePort Service) using creds, and verifies the resolved ARN
// contains wantARNContains.
func assertIMDSCredentialsCallerIdentity(t *testing.T, creds imdsCredentialsResponse, wantARNContains string) {
	t.Helper()

	cmd := exec.Command("aws", "sts", "get-caller-identity",
		"--endpoint-url", "http://localhost:"+imdsAgentHostPort,
	)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.SecretAccessKey,
		"AWS_SESSION_TOKEN="+creds.Token,
		"AWS_REGION=us-east-1",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws sts get-caller-identity: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}
