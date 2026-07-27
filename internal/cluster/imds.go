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
	// imdsName is the name of fjord's IMDS DaemonSet, mirroring the
	// official eks-pod-identity-agent's own naming convention.
	imdsName = "fjord-imds"
	// imdsPort is the port fjord-agent's `serve imds` listens on, bound to
	// the 169.254.169.254 link-local address it adds to each node's
	// loopback interface (see cmd/fjord-agent's `serve imds`). Binding that
	// specific address rather than 0.0.0.0 means it never collides with
	// anything already listening on the node's own IP, even though both
	// use port 80.
	imdsPort = 80
)

// imdsLabels selects fjord-imds's pods, used as both the DaemonSet's pod
// template labels and its selector.
var imdsLabels = map[string]string{"app": imdsName}

// EnsureIMDS deploys fjord's fake EC2 Instance Metadata Service to every
// node as a hostNetwork DaemonSet (fjord-imds) running `serve imds
// --node-role-name nodeRoleName`, so pods with no
// eks.amazonaws.com/role-arn annotation can still obtain node-role
// credentials from 169.254.169.254 as if fjord's kind nodes were real EC2
// instances. It reuses fjord-agent's ServiceAccount and ClusterRole (for
// read/write access to the fjord-principals Secret), so callers must invoke
// EnsureAgent first. It is idempotent; repeated calls update the DaemonSet
// to run image with --node-role-name nodeRoleName.
func EnsureIMDS(ctx context.Context, client kubernetes.Interface, image, nodeRoleName string) error {
	return ensureIMDSDaemonSet(ctx, client, imdsDaemonSet(image, nodeRoleName))
}

// ensureIMDSDaemonSet creates desired if it does not already exist, or
// updates it in place (preserving its ResourceVersion) otherwise.
func ensureIMDSDaemonSet(ctx context.Context, client kubernetes.Interface, desired *appsv1.DaemonSet) error {
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

// imdsDaemonSet builds the desired fjord-imds DaemonSet running image with
// `serve imds --node-role-name nodeRoleName`. It runs with hostNetwork and
// the NET_ADMIN capability so `serve imds` can add 169.254.169.254 to each
// node's loopback interface, and tolerates every taint (matching the
// official eks-pod-identity-agent) so IMDS is reachable from pods scheduled
// on the control-plane node too.
func imdsDaemonSet(image, nodeRoleName string) *appsv1.DaemonSet {
	privileged := false

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: imdsName, Namespace: agent.SystemNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: imdsLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: imdsLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: agentName,
					HostNetwork:        true,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers: []corev1.Container{
						{
							Name:  imdsName,
							Image: image,
							Args: []string{
								"serve", "imds",
								"--port", fmt.Sprintf("%d", imdsPort),
								"--node-role-name", nodeRoleName,
							},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: imdsPort, HostPort: imdsPort, Protocol: corev1.ProtocolTCP},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN"},
								},
							},
						},
					},
				},
			},
		},
	}
}
