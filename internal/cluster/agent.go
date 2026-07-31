package cluster

import (
	"context"
	"fmt"
	"slices"

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

	// AgentNodePort is the NodePort fjord-agent's fake STS API is published
	// on. rask's hostproc runtime shares the host network namespace, so this
	// port is reachable directly on the host without any port mapping.
	AgentNodePort = 30080

	// AgentTLSCertName is the Secret fjord-agent's IRSA injector webhook
	// TLS certificate is stored in and mounted from, when EnsureAgent is
	// called with enableIRSA true. EnsureIRSA populates its content.
	AgentTLSCertName = "fjord-agent-tls"

	// agentTLSMountPath is the path fjord-agent's injector TLS certificate
	// and key are mounted at inside its container.
	agentTLSMountPath = "/etc/fjord/tls"
	// agentInjectorPort is the port fjord-agent's IRSA injector webhook
	// listens on (TLS), matching internal/cluster.EnsureIRSA's
	// MutatingWebhookConfiguration.
	agentInjectorPort = 8443
)

// agentLabels selects fjord-agent's pods, used as both the Deployment's pod
// template labels and its Services' selectors.
var agentLabels = map[string]string{"app": agentName}

// EnsureAgent deploys fjord-agent (the fake STS/IMDS/authenticator server)
// to the kube-system namespace: its RBAC, Deployment, and Services. It is
// idempotent; repeated calls update the Deployment to run image. When
// enableIRSA is true, the Deployment additionally mounts AgentTLSCertName
// and runs fjord-agent's IRSA injector webhook on agentInjectorPort; callers
// enabling it must arrange for EnsureIRSA to populate that Secret.
// awsEndpointURL, when non-empty, is passed to the injector webhook via
// --aws-endpoint-url, so it injects AWS_ENDPOINT_URL into IAM-identity pods
// (see internal/agent.Injector's doc comment); an empty awsEndpointURL
// leaves that injection disabled. When enableLoadBalancer is true,
// fjord-agent additionally runs its LoadBalancer controller (see
// internal/agent.LoadBalancerController's doc comment).
func EnsureAgent(ctx context.Context, client kubernetes.Interface, image string, enableIRSA bool, awsEndpointURL string, enableLoadBalancer bool) error {
	if err := ensureServiceAccount(ctx, client); err != nil {
		return err
	}

	if err := ensureClusterRole(ctx, client); err != nil {
		return err
	}

	if err := ensureClusterRoleBinding(ctx, client); err != nil {
		return err
	}

	if err := ensureDeployment(ctx, client, image, enableIRSA, awsEndpointURL, enableLoadBalancer); err != nil {
		return err
	}

	if err := ensureService(ctx, client, enableIRSA); err != nil {
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
// later phases, ConfigMaps in the same namespace), the ability to create
// TokenReviews for the authenticator webhook, read access to
// ServiceAccounts so its IRSA injector webhook can resolve the
// eks.amazonaws.com/role-arn annotation, the ability to materialize
// ClusterRoleBinding/RoleBinding objects for the EKS access-entries
// facade's AssociateAccessPolicy endpoint (see internal/agent/facade.go's
// materializeAccessPolicyRBAC), read access to ClusterNetworkPolicy custom
// resources plus full access to Namespaces/NetworkPolicies for
// internal/agent's CNPController (which reconciles the former into the
// latter), and read/watch access to elbv2.k8s.aws/v1beta1
// TargetGroupBindings plus control of Services for
// internal/agent.TargetGroupBindingController's mirror-Service emulation.
// The same ClusterRole is also bound to fjord-authenticator's
// ServiceAccount (see EnsureAuthenticatorRBAC), since it needs identical
// read access to the principal registry and access entries.
func ensureClusterRole(ctx context.Context, client kubernetes.Interface) error {
	baseRules := []rbacv1.PolicyRule{
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
		{
			APIGroups: []string{""},
			Resources: []string{"serviceaccounts"},
			Verbs:     []string{"get"},
		},
		{
			APIGroups: []string{"rbac.authorization.k8s.io"},
			Resources: []string{"clusterrolebindings", "rolebindings"},
			Verbs:     []string{"get", "create", "delete"},
		},
		{
			// The bind verb lets fjord-agent create bindings to these
			// built-in roles when materializing an access policy without
			// holding their permissions itself, which RBAC's
			// privilege-escalation prevention would otherwise forbid.
			APIGroups:     []string{"rbac.authorization.k8s.io"},
			Resources:     []string{"clusterroles"},
			Verbs:         []string{"bind"},
			ResourceNames: []string{"cluster-admin", "admin", "edit", "view"},
		},
	}

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: agentName},
		Rules: slices.Concat(
			baseRules,
			clusterNetworkPolicyControllerRules(),
			targetGroupBindingControllerRules(),
		),
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

// clusterNetworkPolicyControllerRules returns the PolicyRules
// internal/agent's CNPController needs: read access to
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy custom resources, read
// access to Namespaces to resolve each policy's subject selector, and full
// control of standard NetworkPolicy objects, the ones it reconciles
// ClusterNetworkPolicies into.
func clusterNetworkPolicyControllerRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"networking.k8s.aws"},
			Resources: []string{"clusternetworkpolicies"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"namespaces"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"networking.k8s.io"},
			Resources: []string{"networkpolicies"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		},
	}
}

// targetGroupBindingControllerRules returns the PolicyRules
// internal/agent.TargetGroupBindingController needs: read access to
// elbv2.k8s.aws/v1beta1 TargetGroupBinding custom resources, and control of
// Services to create/update/delete the mirror LoadBalancer Service it
// emulates each binding with.
func targetGroupBindingControllerRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"services"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
		},
		{
			APIGroups: []string{"elbv2.k8s.aws"},
			Resources: []string{"targetgroupbindings"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
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
func ensureDeployment(ctx context.Context, client kubernetes.Interface, image string, enableIRSA bool, awsEndpointURL string, enableLoadBalancer bool) error {
	desired := agentDeployment(image, enableIRSA, awsEndpointURL, enableLoadBalancer)

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
// When enableIRSA is true, the pod additionally mounts AgentTLSCertName at
// agentTLSMountPath and fjord-agent serves its IRSA injector webhook on
// agentInjectorPort. awsEndpointURL and enableLoadBalancer are forwarded to
// agentContainerArgs; see EnsureAgent's doc comment.
func agentDeployment(image string, enableIRSA bool, awsEndpointURL string, enableLoadBalancer bool) *appsv1.Deployment {
	replicas := int32(1)

	spec := corev1.PodSpec{
		ServiceAccountName: agentName,
		Containers: []corev1.Container{
			{
				Name:  agentName,
				Image: image,
				Args:  agentContainerArgs(enableIRSA, awsEndpointURL, enableLoadBalancer),
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP},
				},
			},
		},
	}

	if enableIRSA {
		spec.Containers[0].Ports = append(spec.Containers[0].Ports,
			corev1.ContainerPort{Name: "injector", ContainerPort: agentInjectorPort, Protocol: corev1.ProtocolTCP})
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "tls", MountPath: agentTLSMountPath, ReadOnly: true},
		}
		spec.Volumes = []corev1.Volume{
			{
				Name: "tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: AgentTLSCertName},
				},
			},
		}
	}

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
				Spec:       spec,
			},
		},
	}
}

// agentContainerArgs returns fjord-agent's `serve api` container args.
// enableIRSA true additionally serves the IRSA injector webhook on
// agentInjectorPort using the certificate mounted at agentTLSMountPath;
// enableIRSA false passes --injector-port 0, which fjord-agent treats as
// "do not serve the injector webhook". A non-empty awsEndpointURL is passed
// via --aws-endpoint-url, so the injector also injects AWS_ENDPOINT_URL into
// IAM-identity pods; an empty awsEndpointURL omits the flag, leaving that
// injection disabled (fjord-agent's own default). enableLoadBalancer true
// additionally passes --enable-loadbalancer, starting fjord-agent's
// LoadBalancer controller.
func agentContainerArgs(enableIRSA bool, awsEndpointURL string, enableLoadBalancer bool) []string {
	args := []string{"serve", "api", "--port", fmt.Sprintf("%d", agentPort)}

	if !enableIRSA {
		args = append(args, "--injector-port", "0")
	} else {
		args = append(args,
			"--injector-port", fmt.Sprintf("%d", agentInjectorPort),
			"--tls-cert-file", agentTLSMountPath+"/tls.crt",
			"--tls-key-file", agentTLSMountPath+"/tls.key",
		)
	}

	if awsEndpointURL != "" {
		args = append(args, "--aws-endpoint-url", awsEndpointURL)
	}

	if enableLoadBalancer {
		args = append(args, "--enable-loadbalancer")
	}

	return args
}

// ensureService creates fjord-agent's ClusterIP Service if it does not
// already exist. When enableIRSA is true, the Service additionally exposes
// agentInjectorPort for the IRSA injector webhook.
func ensureService(ctx context.Context, client kubernetes.Interface, enableIRSA bool) error {
	ports := []corev1.ServicePort{
		{Name: "http", Port: agentPort, TargetPort: intstr.FromInt32(agentPort)},
	}

	if enableIRSA {
		ports = append(ports, corev1.ServicePort{
			Name: "injector", Port: agentInjectorPort, TargetPort: intstr.FromInt32(agentInjectorPort),
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: agent.SystemNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: agentLabels,
			Ports:    ports,
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
// fjord-agent's fake STS API to the host (via internal/provider.Config's
// HostPort) if it does not already exist.
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
				{Name: "http", Port: agentPort, TargetPort: intstr.FromInt32(agentPort), NodePort: AgentNodePort},
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
