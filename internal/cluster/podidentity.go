package cluster

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/sivchari/fjord/internal/agent"
)

const (
	// DefaultPodIdentityAgentImage is the official eks-pod-identity-agent
	// image EnsurePodIdentity deploys, pinned by digest (0.2.1's multi-arch
	// manifest list, resolved via `docker buildx imagetools inspect
	// public.ecr.aws/eks/eks-pod-identity-agent:0.2.1`).
	DefaultPodIdentityAgentImage = "public.ecr.aws/eks/eks-pod-identity-agent@sha256:bc9cf4ae7fcfa4f82a4f7f2d5d825efa952d8a4e23dfd7ba923c02dcf68dadc8"

	// podIdentityAgentName is the name of fjord's eks-pod-identity-agent
	// DaemonSet, prefixed like every other fjord-managed resource (compare
	// imdsName in imds.go).
	podIdentityAgentName = "fjord-eks-pod-identity-agent"

	// podIdentityAgentHostIP is the well-known EKS Pod Identity link-local
	// address the agent's own `initialize` initContainer configures on each
	// node, matching the real eks-pod-identity-agent's deployment.
	podIdentityAgentHostIP = "169.254.170.23"
	// podIdentityAgentPort is the port eks-pod-identity-agent listens on,
	// bound to podIdentityAgentHostIP.
	podIdentityAgentPort = 80
	// podIdentityAgentProbePort is eks-pod-identity-agent's health/readiness
	// probe port, matching the official chart's default.
	podIdentityAgentProbePort = 2703
	// podIdentityAgentRegion is the AWS_REGION eks-pod-identity-agent's AWS
	// SDK client resolves, matching every other dummy region value fjord
	// reports (see internal/agent/imds.go's imdsRegion).
	podIdentityAgentRegion = "us-east-1"
	// podIdentityAgentSignID and podIdentityAgentSignValue are the dummy
	// static credentials the agent signs its (signature-ignored)
	// AssumeRoleForPodIdentity calls to fjord's fake eks-auth with. fjord
	// ignores SigV4, so the values only need to be non-empty; they are
	// deliberately not AWS-key-shaped.
	podIdentityAgentSignID    = "fjord-pod-identity-agent"
	podIdentityAgentSignValue = "fjord-pod-identity-agent-dummy"
)

// podIdentityAgentLabels selects fjord's eks-pod-identity-agent DaemonSet's
// pods, used as both its pod template labels and its selector.
var podIdentityAgentLabels = map[string]string{"app": podIdentityAgentName}

// EnsurePodIdentity deploys fjord's local emulation of EKS Pod Identity: the
// official aws/eks-pod-identity-agent as a hostNetwork DaemonSet, serving
// pod credential requests on the well-known link-local address
// podIdentityAgentHostIP and forwarding them, via its own --endpoint flag,
// to fjord-agent's EKS Auth API emulation
// (internal/agent's POST /clusters/{name}/assume-role-for-pod-identity
// handler) instead of the real EKS Auth API.
//
// EKS Pod Identity credential *injection* into pods -- the
// AWS_CONTAINER_CREDENTIALS_FULL_URI env var and a projected ServiceAccount
// token volume for audience "pods.eks.amazonaws.com" -- is, on real EKS,
// performed by the same amazon-eks-pod-identity-webhook EnsureIRSA deploys
// (Phase 4), but gated on a completely different mechanism than IRSA's
// role-arn annotation: the webhook only mutates ServiceAccounts listed in a
// JSON config file it watches via its own
// --watch-container-credentials-config flag (see
// aws/amazon-eks-pod-identity-webhook's pkg/containercredentials package),
// which on real EKS the control plane keeps in sync with
// PodIdentityAssociations. fjord has no such control plane, so
// EnsurePodIdentity does not enable that webhook flag; fjord's own Injector
// webhook (internal/agent.Injector, already deployed by EnsureIRSA) performs
// this injection itself instead, gated on internal/agent.PodIdentityStore.
// EnsurePodIdentity therefore only needs to deploy the credential-serving
// DaemonSet below, not reconfigure the webhook.
//
// EnsurePodIdentity is idempotent; repeated calls update the DaemonSet to
// run agentImage. clusterName is passed to eks-pod-identity-agent's required
// --cluster-name flag, which it uses only to fill the {name} path segment of
// the URL it calls; fjord's EKS Auth API emulation does not otherwise use
// it.
func EnsurePodIdentity(ctx context.Context, client kubernetes.Interface, agentImage, clusterName string) error {
	return ensurePodIdentityDaemonSet(ctx, client, podIdentityAgentDaemonSet(agentImage, clusterName))
}

// ensurePodIdentityDaemonSet creates desired if it does not already exist,
// or updates it in place (preserving its ResourceVersion) otherwise.
func ensurePodIdentityDaemonSet(ctx context.Context, client kubernetes.Interface, desired *appsv1.DaemonSet) error {
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

// podIdentityAgentDaemonSet builds the desired eks-pod-identity-agent
// DaemonSet running agentImage, matching the official Helm chart's own
// daemonset.yaml: a privileged `initialize` initContainer configuring
// podIdentityAgentHostIP, and a `server` main container running with only
// the CAP_NET_BIND_SERVICE capability, forwarding credential requests to
// fjord-agent's EKS Auth API emulation at fjord-agent.kube-system.svc. It
// runs with hostNetwork and tolerates every taint (matching fjord-imds), so
// it is reachable from pods scheduled on the control-plane node too.
func podIdentityAgentDaemonSet(agentImage, clusterName string) *appsv1.DaemonSet {
	privileged := true

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityAgentName, Namespace: agent.SystemNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podIdentityAgentLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podIdentityAgentLabels},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					// A hostNetwork pod uses the host's resolver by default,
					// which cannot resolve the in-cluster fjord-agent Service
					// the agent's --endpoint points at.
					// ClusterFirstWithHostNet routes DNS through CoreDNS so
					// that name resolves.
					DNSPolicy: corev1.DNSClusterFirstWithHostNet,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					InitContainers: podIdentityAgentInitContainers(agentImage, privileged),
					Containers: []corev1.Container{
						{
							Name:    podIdentityAgentName,
							Image:   agentImage,
							Command: []string{"/go-runner", "/eks-pod-identity-agent", "server"},
							Args: []string{
								"--port", fmt.Sprintf("%d", podIdentityAgentPort),
								"--cluster-name", clusterName,
								"--probe-port", fmt.Sprintf("%d", podIdentityAgentProbePort),
								"--endpoint", fmt.Sprintf("http://%s:%d", serviceFQDN(agentName), agentPort),
								"--bind-hosts", podIdentityAgentHostIP,
							},
							Env: []corev1.EnvVar{
								{Name: "AWS_REGION", Value: podIdentityAgentRegion},
								// The agent signs its AssumeRoleForPodIdentity
								// call to fjord's fake eks-auth with SigV4.
								// fjord ignores the signature, so static dummy
								// credentials suffice and spare the agent from
								// resolving its own credentials via IMDS.
								{Name: "AWS_ACCESS_KEY_ID", Value: podIdentityAgentSignID},
								{Name: "AWS_SECRET_ACCESS_KEY", Value: podIdentityAgentSignValue},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "proxy",
									ContainerPort: podIdentityAgentPort,
									HostPort:      podIdentityAgentPort,
									HostIP:        podIdentityAgentHostIP,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"CAP_NET_BIND_SERVICE"},
								},
							},
						},
					},
				},
			},
		},
	}
}

// podIdentityAgentInitContainers returns the agent's init container, which
// adds the link-local address the agent binds to (matching the upstream
// chart's privileged initialize step).
func podIdentityAgentInitContainers(agentImage string, privileged bool) []corev1.Container {
	return []corev1.Container{
		{
			Name:    podIdentityAgentName + "-init",
			Image:   agentImage,
			Command: []string{"/go-runner", "/eks-pod-identity-agent", "initialize"},
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
		},
	}
}
