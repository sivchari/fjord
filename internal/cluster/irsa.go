package cluster

import (
	"context"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/pki"
)

const (
	// DefaultPodIdentityWebhookImage is the amazon-eks-pod-identity-webhook
	// image EnsureIRSA deploys, pinned by digest (v0.6.17's multi-arch
	// manifest list, resolved via `docker buildx imagetools inspect
	// public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.6.17`).
	DefaultPodIdentityWebhookImage = "public.ecr.aws/eks/amazon-eks-pod-identity-webhook@sha256:3071570e8e8cba6d5aff01e86d52bc91025a0578782aa9cd2b681480845b0a50"

	injectorWebhookName = "fjord-injector"
	injectorWebhookPath = "/inject"

	podIdentityWebhookName       = "pod-identity-webhook"
	podIdentityWebhookCertSecret = "pod-identity-webhook-cert"
	podIdentityWebhookPath       = "/mutate"
	podIdentityWebhookPort       = 443
	podIdentityWebhookMountPath  = "/etc/webhook/certs"

	// namespaceNameLabel is the well-known label kube-apiserver sets on
	// every Namespace to its own name, used by both webhooks'
	// namespaceSelector to exclude kube-system.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// podIdentityWebhookLabels selects the pod-identity-webhook Deployment's
// pods, used as both its pod template labels and its Service's selector.
var podIdentityWebhookLabels = map[string]string{"app": podIdentityWebhookName}

// EnsureIRSA deploys IRSA support to the cluster:
//
//   - The official amazon-eks-pod-identity-webhook, which injects
//     AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE, and a projected
//     ServiceAccount token volume into pods running as a ServiceAccount
//     carrying the eks.amazonaws.com/role-arn annotation.
//   - fjord's own injector webhook (internal/agent.Injector, served by
//     fjord-agent), which additionally injects AWS_ENDPOINT_URL_STS into
//     those same pods so their AWS SDK calls fjord-agent's fake STS instead
//     of the real one.
//
// Both webhooks' server certificates are issued by ca; ca's certificate is
// embedded as every webhook's caBundle. EnsureIRSA is idempotent. webhookImage
// is the amazon-eks-pod-identity-webhook image to deploy.
func EnsureIRSA(ctx context.Context, client kubernetes.Interface, ca *pki.CA, webhookImage string) error {
	if err := ensureAgentTLSSecret(ctx, client, ca); err != nil {
		return err
	}

	if err := ensureInjectorWebhookConfiguration(ctx, client, ca); err != nil {
		return err
	}

	if err := ensurePodIdentityWebhookCertSecret(ctx, client, ca); err != nil {
		return err
	}

	if err := ensurePodIdentityWebhookRBAC(ctx, client); err != nil {
		return err
	}

	if err := ensurePodIdentityWebhookDeployment(ctx, client, webhookImage); err != nil {
		return err
	}

	if err := ensurePodIdentityWebhookService(ctx, client); err != nil {
		return err
	}

	return ensurePodIdentityWebhookConfiguration(ctx, client, ca)
}

// ensureAgentTLSSecret issues fjord-agent's injector webhook server
// certificate from ca and stores it in AgentTLSCertName.
func ensureAgentTLSSecret(ctx context.Context, client kubernetes.Interface, ca *pki.CA) error {
	cert, err := ca.IssueServerCert(serviceFQDN(agentName), []string{serviceFQDN(agentName), serviceFQDNCluster(agentName)})
	if err != nil {
		return fmt.Errorf("issue %q server certificate: %w", agentName, err)
	}

	return ensureTLSSecret(ctx, client, AgentTLSCertName, cert)
}

// ensurePodIdentityWebhookCertSecret issues pod-identity-webhook's server
// certificate from ca and stores it in podIdentityWebhookCertSecret.
func ensurePodIdentityWebhookCertSecret(ctx context.Context, client kubernetes.Interface, ca *pki.CA) error {
	cert, err := ca.IssueServerCert(
		serviceFQDN(podIdentityWebhookName),
		[]string{serviceFQDN(podIdentityWebhookName), serviceFQDNCluster(podIdentityWebhookName)},
	)
	if err != nil {
		return fmt.Errorf("issue %q server certificate: %w", podIdentityWebhookName, err)
	}

	return ensureTLSSecret(ctx, client, podIdentityWebhookCertSecret, cert)
}

// serviceFQDN returns name's in-cluster DNS name within kube-system.
func serviceFQDN(name string) string {
	return fmt.Sprintf("%s.%s.svc", name, agent.SystemNamespace)
}

// serviceFQDNCluster returns name's fully-qualified in-cluster DNS name
// within kube-system.
func serviceFQDNCluster(name string) string {
	return serviceFQDN(name) + ".cluster.local"
}

// ensureTLSSecret creates a kubernetes.io/tls Secret named name in
// kube-system holding cert, or updates it in place if it already exists.
func ensureTLSSecret(ctx context.Context, client kubernetes.Interface, name string, cert *pki.ServerCert) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.SystemNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       cert.CertPEM,
			corev1.TLSPrivateKeyKey: cert.KeyPEM,
		},
	}

	_, err := client.CoreV1().Secrets(agent.SystemNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q secret: %w", name, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.CoreV1().Secrets(agent.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q secret: %w", name, getErr)
		}

		existing.Type = secret.Type
		existing.Data = secret.Data

		if _, updateErr := client.CoreV1().Secrets(agent.SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q secret: %w", name, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q secret: %w", name, err)
	}

	return nil
}

// excludeSystemNamespaceSelector returns a namespaceSelector matching every
// namespace except kube-system, so fjord's IRSA webhooks never mutate the
// system pods (including their own) running there.
func excludeSystemNamespaceSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      namespaceNameLabel,
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{agent.SystemNamespace},
			},
		},
	}
}

// podCreateRule is the RuleWithOperations both IRSA MutatingWebhookConfigurations
// match: Pod CREATE.
func podCreateRule() admissionregistrationv1.RuleWithOperations {
	scope := admissionregistrationv1.AllScopes

	return admissionregistrationv1.RuleWithOperations{
		Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"pods"},
			Scope:       &scope,
		},
	}
}

// ensureInjectorWebhookConfiguration creates or updates the fjord-injector
// MutatingWebhookConfiguration, pointing it at fjord-agent's injector
// webhook and trusting ca.
func ensureInjectorWebhookConfiguration(ctx context.Context, client kubernetes.Interface, ca *pki.CA) error {
	path := injectorWebhookPath
	port := int32(agentInjectorPort)
	failurePolicy := admissionregistrationv1.Ignore
	sideEffects := admissionregistrationv1.SideEffectClassNone

	desired := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: injectorWebhookName},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: injectorWebhookName + ".fjord.dev",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Name:      agentName,
						Namespace: agent.SystemNamespace,
						Path:      &path,
						Port:      &port,
					},
					CABundle: ca.CertPEM,
				},
				Rules:                   []admissionregistrationv1.RuleWithOperations{podCreateRule()},
				NamespaceSelector:       excludeSystemNamespaceSelector(),
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}

	return ensureMutatingWebhookConfiguration(ctx, client, desired)
}

// ensurePodIdentityWebhookConfiguration creates or updates the
// pod-identity-webhook MutatingWebhookConfiguration, pointing it at the
// pod-identity-webhook Service and trusting ca.
func ensurePodIdentityWebhookConfiguration(ctx context.Context, client kubernetes.Interface, ca *pki.CA) error {
	path := podIdentityWebhookPath
	port := int32(podIdentityWebhookPort)
	failurePolicy := admissionregistrationv1.Ignore
	sideEffects := admissionregistrationv1.SideEffectClassNone

	desired := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: podIdentityWebhookName + ".amazonaws.com",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Name:      podIdentityWebhookName,
						Namespace: agent.SystemNamespace,
						Path:      &path,
						Port:      &port,
					},
					CABundle: ca.CertPEM,
				},
				Rules:             []admissionregistrationv1.RuleWithOperations{podCreateRule()},
				NamespaceSelector: excludeSystemNamespaceSelector(),
				FailurePolicy:     &failurePolicy,
				SideEffects:       &sideEffects,
				// pod-identity-webhook v0.6.17 only sets the response
				// TypeMeta for v1beta1 AdmissionReview; requesting v1 makes
				// the API server reject its empty-GVK response. The upstream
				// deployment declares v1beta1 for the same reason.
				AdmissionReviewVersions: []string{"v1beta1"},
			},
		},
	}

	return ensureMutatingWebhookConfiguration(ctx, client, desired)
}

// ensureMutatingWebhookConfiguration creates desired if it does not already
// exist, or updates it in place (preserving its ResourceVersion) otherwise.
func ensureMutatingWebhookConfiguration(ctx context.Context, client kubernetes.Interface, desired *admissionregistrationv1.MutatingWebhookConfiguration) error {
	_, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q mutating webhook configuration: %w", desired.Name, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, desired.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q mutating webhook configuration: %w", desired.Name, getErr)
		}

		existing.Webhooks = desired.Webhooks

		if _, updateErr := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q mutating webhook configuration: %w", desired.Name, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q mutating webhook configuration: %w", desired.Name, err)
	}

	return nil
}

// ensurePodIdentityWebhookRBAC creates pod-identity-webhook's ServiceAccount,
// ClusterRole, and ClusterRoleBinding if they do not already exist,
// mirroring amazon-eks-pod-identity-webhook's own deploy/auth.yaml.
func ensurePodIdentityWebhookRBAC(ctx context.Context, client kubernetes.Interface) error {
	if err := ensurePodIdentityWebhookServiceAccount(ctx, client); err != nil {
		return err
	}

	if err := ensurePodIdentityWebhookClusterRole(ctx, client); err != nil {
		return err
	}

	return ensurePodIdentityWebhookClusterRoleBinding(ctx, client)
}

// ensurePodIdentityWebhookServiceAccount creates pod-identity-webhook's
// ServiceAccount if it does not already exist.
func ensurePodIdentityWebhookServiceAccount(ctx context.Context, client kubernetes.Interface) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName, Namespace: agent.SystemNamespace},
	}

	_, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service account: %w", podIdentityWebhookName, err)
	}

	return nil
}

// ensurePodIdentityWebhookClusterRole creates pod-identity-webhook's
// ClusterRole if it does not already exist, granting it read access to
// ServiceAccounts so it can resolve the eks.amazonaws.com/role-arn
// annotation.
func ensurePodIdentityWebhookClusterRole(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"serviceaccounts"},
				Verbs:     []string{"get", "watch", "list"},
			},
		},
	}

	_, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q cluster role: %w", podIdentityWebhookName, err)
	}

	return nil
}

// ensurePodIdentityWebhookClusterRoleBinding creates pod-identity-webhook's
// ClusterRoleBinding if it does not already exist, binding its ClusterRole
// to its ServiceAccount.
func ensurePodIdentityWebhookClusterRoleBinding(ctx context.Context, client kubernetes.Interface) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     podIdentityWebhookName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: podIdentityWebhookName, Namespace: agent.SystemNamespace},
		},
	}

	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q cluster role binding: %w", podIdentityWebhookName, err)
	}

	return nil
}

// ensurePodIdentityWebhookDeployment creates pod-identity-webhook's
// Deployment if it does not already exist, or updates it in place
// (preserving its ResourceVersion) to run image otherwise.
func ensurePodIdentityWebhookDeployment(ctx context.Context, client kubernetes.Interface, image string) error {
	desired := podIdentityWebhookDeployment(image)

	_, err := client.AppsV1().Deployments(agent.SystemNamespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q deployment: %w", podIdentityWebhookName, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, podIdentityWebhookName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q deployment: %w", podIdentityWebhookName, getErr)
		}

		existing.Spec = desired.Spec

		if _, updateErr := client.AppsV1().Deployments(agent.SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q deployment: %w", podIdentityWebhookName, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q deployment: %w", podIdentityWebhookName, err)
	}

	return nil
}

// podIdentityWebhookDeployment builds the desired pod-identity-webhook
// Deployment running image, matching amazon-eks-pod-identity-webhook's own
// deploy/deployment-base.yaml args.
func podIdentityWebhookDeployment(image string) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName, Namespace: agent.SystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: podIdentityWebhookLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podIdentityWebhookLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: podIdentityWebhookName,
					Containers: []corev1.Container{
						{
							Name:  podIdentityWebhookName,
							Image: image,
							// The image's entrypoint is /go-runner, a launcher
							// that does not understand the webhook flags. Set
							// command to the webhook binary directly, as the
							// upstream deployment does.
							Command: []string{"/webhook"},
							Args: []string{
								"--in-cluster=false",
								"--port=443",
								"--tls-key=" + podIdentityWebhookMountPath + "/tls.key",
								"--tls-cert=" + podIdentityWebhookMountPath + "/tls.crt",
							},
							Ports: []corev1.ContainerPort{
								{Name: "https", ContainerPort: podIdentityWebhookPort, Protocol: corev1.ProtocolTCP},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "cert", MountPath: podIdentityWebhookMountPath, ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: podIdentityWebhookCertSecret},
							},
						},
					},
				},
			},
		},
	}
}

// ensurePodIdentityWebhookService creates pod-identity-webhook's ClusterIP
// Service if it does not already exist.
func ensurePodIdentityWebhookService(ctx context.Context, client kubernetes.Interface) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityWebhookName, Namespace: agent.SystemNamespace},
		Spec: corev1.ServiceSpec{
			Selector: podIdentityWebhookLabels,
			Ports: []corev1.ServicePort{
				{Name: "https", Port: podIdentityWebhookPort, TargetPort: intstr.FromInt32(podIdentityWebhookPort)},
			},
		},
	}

	_, err := client.CoreV1().Services(agent.SystemNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service: %w", podIdentityWebhookName, err)
	}

	return nil
}
