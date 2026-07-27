package cluster

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/pki"
)

const testWebhookImage = "public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.6.17"

// testCA returns a fresh CA for irsa_test.go's cases.
func testCA(t *testing.T) *pki.CA {
	t.Helper()

	ca, err := pki.NewCA("fjord-test-ca")
	if err != nil {
		t.Fatalf("pki.NewCA: %v", err)
	}

	return ca
}

func TestEnsureIRSA(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ca := testCA(t)

	if err := EnsureIRSA(context.Background(), client, ca, testWebhookImage); err != nil {
		t.Fatalf("EnsureIRSA: %v", err)
	}

	assertIRSAResources(t, client, ca, testWebhookImage)
}

func TestEnsureIRSAIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ca := testCA(t)

	for range 2 {
		if err := EnsureIRSA(context.Background(), client, ca, testWebhookImage); err != nil {
			t.Fatalf("EnsureIRSA: %v", err)
		}
	}

	assertIRSAResources(t, client, ca, testWebhookImage)
}

func TestEnsureIRSAUpdatesImage(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ca := testCA(t)

	if err := EnsureIRSA(context.Background(), client, ca, testWebhookImage); err != nil {
		t.Fatalf("EnsureIRSA: %v", err)
	}

	const updatedImage = "public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.7.0"

	if err := EnsureIRSA(context.Background(), client, ca, updatedImage); err != nil {
		t.Fatalf("EnsureIRSA: %v", err)
	}

	assertIRSAResources(t, client, ca, updatedImage)
}

// assertIRSAResources verifies every resource EnsureIRSA creates exists in
// client with the expected shape, and that both webhooks trust ca.
func assertIRSAResources(t *testing.T, client *fake.Clientset, ca *pki.CA, webhookImage string) {
	t.Helper()

	ctx := context.Background()

	assertTLSSecret(t, client, AgentTLSCertName, ca)
	assertTLSSecret(t, client, podIdentityWebhookCertSecret, ca)

	if _, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Get(ctx, podIdentityWebhookName, metav1.GetOptions{}); err != nil {
		t.Errorf("get service account: %v", err)
	}

	if _, err := client.RbacV1().ClusterRoles().Get(ctx, podIdentityWebhookName, metav1.GetOptions{}); err != nil {
		t.Errorf("get cluster role: %v", err)
	}

	if _, err := client.RbacV1().ClusterRoleBindings().Get(ctx, podIdentityWebhookName, metav1.GetOptions{}); err != nil {
		t.Errorf("get cluster role binding: %v", err)
	}

	deployment, err := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, podIdentityWebhookName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != webhookImage {
		t.Errorf("deployment image = %q, want %q", got, webhookImage)
	}

	if _, err := client.CoreV1().Services(agent.SystemNamespace).Get(ctx, podIdentityWebhookName, metav1.GetOptions{}); err != nil {
		t.Errorf("get service: %v", err)
	}

	assertMutatingWebhookConfiguration(t, client, injectorWebhookName, agentName, injectorWebhookPath, ca)
	assertMutatingWebhookConfiguration(t, client, podIdentityWebhookName, podIdentityWebhookName, podIdentityWebhookPath, ca)
}

// assertTLSSecret verifies a kubernetes.io/tls Secret named name exists in
// kube-system and its certificate is trusted by ca.
func assertTLSSecret(t *testing.T, client *fake.Clientset, name string, ca *pki.CA) {
	t.Helper()

	secret, err := client.CoreV1().Secrets(agent.SystemNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %q secret: %v", name, err)
	}

	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("%q secret type = %q, want %q", name, secret.Type, corev1.SecretTypeTLS)
	}

	if len(secret.Data[corev1.TLSCertKey]) == 0 {
		t.Errorf("%q secret carries no %q", name, corev1.TLSCertKey)
	}

	if len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Errorf("%q secret carries no %q", name, corev1.TLSPrivateKeyKey)
	}

	if certPEM := secret.Data[corev1.TLSCertKey]; bytes.Equal(certPEM, ca.CertPEM) {
		t.Errorf("%q secret stores the CA certificate itself, want a leaf certificate issued by it", name)
	}
}

// assertMutatingWebhookConfiguration verifies the MutatingWebhookConfiguration
// named name exists, targets serviceName at path, and trusts ca.
func assertMutatingWebhookConfiguration(t *testing.T, client *fake.Clientset, name, serviceName, path string, ca *pki.CA) {
	t.Helper()

	webhook, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %q mutating webhook configuration: %v", name, err)
	}

	if len(webhook.Webhooks) != 1 {
		t.Fatalf("%q webhooks = %d, want 1", name, len(webhook.Webhooks))
	}

	wh := webhook.Webhooks[0]

	if wh.ClientConfig.Service == nil {
		t.Fatalf("%q webhook has no service client config", name)
	}

	if got := wh.ClientConfig.Service.Name; got != serviceName {
		t.Errorf("%q webhook service = %q, want %q", name, got, serviceName)
	}

	if wh.ClientConfig.Service.Path == nil || *wh.ClientConfig.Service.Path != path {
		t.Errorf("%q webhook path = %v, want %q", name, wh.ClientConfig.Service.Path, path)
	}

	if !bytes.Equal(wh.ClientConfig.CABundle, ca.CertPEM) {
		t.Errorf("%q webhook caBundle does not match the CA certificate", name)
	}

	if wh.FailurePolicy == nil || *wh.FailurePolicy != "Ignore" {
		t.Errorf("%q webhook failurePolicy = %v, want Ignore", name, wh.FailurePolicy)
	}

	if wh.NamespaceSelector == nil {
		t.Fatalf("%q webhook has no namespaceSelector", name)
	}

	assertExcludesSystemNamespace(t, name, wh.NamespaceSelector)
}

// assertExcludesSystemNamespace verifies selector excludes kube-system.
func assertExcludesSystemNamespace(t *testing.T, webhookName string, selector *metav1.LabelSelector) {
	t.Helper()

	for _, expr := range selector.MatchExpressions {
		if expr.Key != namespaceNameLabel || expr.Operator != metav1.LabelSelectorOpNotIn {
			continue
		}

		for _, v := range expr.Values {
			if v == agent.SystemNamespace {
				return
			}
		}
	}

	t.Errorf("%q webhook namespaceSelector = %+v, want it to exclude %q", webhookName, selector, agent.SystemNamespace)
}
