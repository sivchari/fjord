package cluster

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/sivchari/fjord/internal/agent"
)

const (
	// authenticatorPort is the port fjord-authenticator serves its
	// TokenReview webhook on, matching the server URL in the webhook
	// kubeconfig internal/authn.Stage writes.
	authenticatorPort = 9443
	// authenticatorAuthnDir is the node directory internal/authn.Stage's
	// mount delivers the authenticator's TLS material to.
	authenticatorAuthnDir = "/etc/fjord/authn"
	// controlPlaneNodeLabel selects the control-plane node the authenticator
	// must run on, so its hostNetwork listener shares the API server's
	// network namespace and the webhook's "localhost" server reaches it.
	controlPlaneNodeLabel = "node-role.kubernetes.io/control-plane"
)

// authenticatorLabels selects fjord-authenticator's pods.
var authenticatorLabels = map[string]string{"app": AuthenticatorServiceAccountName}

// EnsureAuthenticator deploys fjord-authenticator as a hostNetwork DaemonSet
// pinned to the control-plane node, running image as `serve authenticator`.
// It runs hostNetwork so the API server (itself a hostNetwork static pod)
// reaches its TLS listener at localhost:9443, the address the webhook
// kubeconfig internal/authn.Stage staged points at. Unlike a static pod, a
// scheduled pod establishes the node/ServiceAccount relationship the Node
// Authorizer requires to issue its projected token, so kubelet injects the
// ServiceAccount token automatically. It reuses fjord-authenticator's
// ServiceAccount (see EnsureAuthenticatorRBAC), so callers must invoke that
// first. It is idempotent.
func EnsureAuthenticator(ctx context.Context, client kubernetes.Interface, image string) error {
	return ensureAuthenticatorDaemonSet(ctx, client, authenticatorDaemonSet(image))
}

// ensureAuthenticatorDaemonSet creates desired if it does not already exist,
// or updates it in place (preserving its ResourceVersion) otherwise.
func ensureAuthenticatorDaemonSet(ctx context.Context, client kubernetes.Interface, desired *appsv1.DaemonSet) error {
	_, err := client.AppsV1().DaemonSets(agent.SystemNamespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q daemonset: %w", desired.Name, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.AppsV1().DaemonSets(agent.SystemNamespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q daemonset: %w", desired.Name, getErr)
		}

		existing.Spec = desired.Spec

		if _, updateErr := client.AppsV1().DaemonSets(agent.SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q daemonset: %w", desired.Name, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q daemonset: %w", desired.Name, err)
	}

	return nil
}

// authenticatorDaemonSet builds the desired fjord-authenticator DaemonSet.
func authenticatorDaemonSet(image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: AuthenticatorServiceAccountName, Namespace: agent.SystemNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: authenticatorLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: authenticatorLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: AuthenticatorServiceAccountName,
					HostNetwork:        true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					NodeSelector:       map[string]string{controlPlaneNodeLabel: ""},
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers: []corev1.Container{
						{
							Name:  AuthenticatorServiceAccountName,
							Image: image,
							Args: []string{
								"serve", "authenticator",
								"--port", fmt.Sprintf("%d", authenticatorPort),
								"--tls-cert-file", authenticatorAuthnDir + "/tls.crt",
								"--tls-key-file", authenticatorAuthnDir + "/tls.key",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "authn", MountPath: authenticatorAuthnDir, ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "authn",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: authenticatorAuthnDir},
							},
						},
					},
				},
			},
		},
	}
}

// AuthenticatorServiceAccountName is the name of the ServiceAccount
// fjord-authenticator's DaemonSet runs as.
const AuthenticatorServiceAccountName = "fjord-authenticator"

// authenticatorClusterRoleBindingName binds AuthenticatorServiceAccountName
// to the fjord-agent ClusterRole (see ensureClusterRole), giving
// fjord-authenticator the same read access to the principal registry and
// access entries fjord-agent's own fake STS and EKS API facade use.
const authenticatorClusterRoleBindingName = "fjord-authenticator"

// EnsureAuthenticatorRBAC creates the ServiceAccount and ClusterRoleBinding
// fjord-authenticator needs to read the principal registry and access entries
// via an in-cluster Kubernetes client, if they do not already exist. It
// reuses fjord-agent's own ClusterRole, so callers must invoke EnsureAgent
// first, and must run before EnsureAuthenticator deploys the DaemonSet that
// runs as this ServiceAccount.
func EnsureAuthenticatorRBAC(ctx context.Context, client kubernetes.Interface) error {
	if err := ensureAuthenticatorServiceAccount(ctx, client); err != nil {
		return err
	}

	return ensureAuthenticatorClusterRoleBinding(ctx, client)
}

// ensureAuthenticatorServiceAccount creates AuthenticatorServiceAccountName
// if it does not already exist.
func ensureAuthenticatorServiceAccount(ctx context.Context, client kubernetes.Interface) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AuthenticatorServiceAccountName,
			Namespace: agent.SystemNamespace,
		},
	}

	_, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service account: %w", AuthenticatorServiceAccountName, err)
	}

	return nil
}

// ensureAuthenticatorClusterRoleBinding creates the ClusterRoleBinding
// binding AuthenticatorServiceAccountName to fjord-agent's ClusterRole, if
// it does not already exist.
func ensureAuthenticatorClusterRoleBinding(ctx context.Context, client kubernetes.Interface) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: authenticatorClusterRoleBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     agentName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      AuthenticatorServiceAccountName,
				Namespace: agent.SystemNamespace,
			},
		},
	}

	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q cluster role binding: %w", authenticatorClusterRoleBindingName, err)
	}

	return nil
}
