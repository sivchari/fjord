package cluster

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/sivchari/fjord/internal/agent"
)

const (
	// agentName is the name of every namespaced fjord-agent resource
	// (ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment, and its
	// ClusterIP Service).
	agentName = "fjord-agent"
	// agentNodePortServiceName is the name of the NodePort Service that
	// publishes fjord-agent's fake STS API to the host.
	agentNodePortServiceName = "fjord-agent-nodeport"
	// agentPort is the port fjord-agent's fake STS API listens on.
	agentPort = 8080
	// agentNodePort is the NodePort fjord-agent's fake STS API is published
	// on, matching internal/kind.Config's ExtraPortMappings target.
	agentNodePort = 30080
)

// agentLabels selects fjord-agent's pods, used as both the Deployment's pod
// template labels and its Services' selectors.
var agentLabels = map[string]string{"app": agentName}

// EnsureAgent deploys fjord-agent (the fake STS/IMDS/authenticator server)
// to the kube-system namespace: its RBAC, Deployment, and Services. It is
// idempotent; repeated calls update the Deployment to run image.
func EnsureAgent(ctx context.Context, client kubernetes.Interface, image string) error {
	if err := ensureServiceAccount(ctx, client); err != nil {
		return err
	}

	if err := ensureClusterRole(ctx, client); err != nil {
		return err
	}

	if err := ensureClusterRoleBinding(ctx, client); err != nil {
		return err
	}

	if err := ensureDeployment(ctx, client, image); err != nil {
		return err
	}

	if err := ensureService(ctx, client); err != nil {
		return err
	}

	return ensureNodePortService(ctx, client)
}

// ensureServiceAccount creates fjord-agent's ServiceAccount if it does not
// already exist.
func ensureServiceAccount(ctx context.Context, client kubernetes.Interface) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: agent.SystemNamespace,
		},
	}

	_, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service account: %w", agentName, err)
	}

	return nil
}

// ensureClusterRole creates fjord-agent's ClusterRole if it does not already
// exist, granting it access to the principal registry Secret (and, for
// later phases, ConfigMaps in the same namespace) plus the ability to
// create TokenReviews for the authenticator webhook.
func ensureClusterRole(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: agentName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets", "configmaps"},
				Verbs:     []string{"get", "list", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"authentication.k8s.io"},
				Resources: []string{"tokenreviews"},
				Verbs:     []string{"create"},
			},
		},
	}

	_, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q cluster role: %w", agentName, err)
	}

	return nil
}

// ensureClusterRoleBinding creates fjord-agent's ClusterRoleBinding if it
// does not already exist, binding its ClusterRole to its ServiceAccount.
func ensureClusterRoleBinding(ctx context.Context, client kubernetes.Interface) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agentName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     agentName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      agentName,
				Namespace: agent.SystemNamespace,
			},
		},
	}

	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q cluster role binding: %w", agentName, err)
	}

	return nil
}

// ensureDeployment creates fjord-agent's Deployment if it does not already
// exist, or updates it in place (preserving its ResourceVersion) to run
// image otherwise.
func ensureDeployment(ctx context.Context, client kubernetes.Interface, image string) error {
	desired := agentDeployment(image)

	_, err := client.AppsV1().Deployments(agent.SystemNamespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q deployment: %w", agentName, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, agentName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q deployment: %w", agentName, getErr)
		}

		existing.Spec = desired.Spec

		if _, updateErr := client.AppsV1().Deployments(agent.SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q deployment: %w", agentName, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q deployment: %w", agentName, err)
	}

	return nil
}

// agentDeployment builds the desired fjord-agent Deployment running image.
func agentDeployment(image string) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: agent.SystemNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: agentLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: agentLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: agentName,
					Containers: []corev1.Container{
						{
							Name:  agentName,
							Image: image,
							Args:  []string{"serve", "api", "--port", fmt.Sprintf("%d", agentPort)},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP},
							},
						},
					},
				},
			},
		},
	}
}

// ensureService creates fjord-agent's ClusterIP Service if it does not
// already exist.
func ensureService(ctx context.Context, client kubernetes.Interface) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: agent.SystemNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: agentLabels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: agentPort, TargetPort: intstr.FromInt32(agentPort)},
			},
		},
	}

	_, err := client.CoreV1().Services(agent.SystemNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service: %w", agentName, err)
	}

	return nil
}

// ensureNodePortService creates the NodePort Service that publishes
// fjord-agent's fake STS API to the host (via internal/kind.Config's
// ExtraPortMappings) if it does not already exist.
func ensureNodePortService(ctx context.Context, client kubernetes.Interface) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentNodePortServiceName,
			Namespace: agent.SystemNamespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: agentLabels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: agentPort, TargetPort: intstr.FromInt32(agentPort), NodePort: agentNodePort},
			},
		},
	}

	_, err := client.CoreV1().Services(agent.SystemNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service: %w", agentNodePortServiceName, err)
	}

	return nil
}
