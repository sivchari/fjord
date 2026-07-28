package cluster

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/sivchari/fjord/internal/agent"
)

const (
	// DefaultKumoImage is the kumo (LocalStack-compatible AWS emulator)
	// image EnsureKumo deploys, pinned by digest (latest's multi-arch
	// manifest list, resolved via `docker buildx imagetools inspect
	// ghcr.io/sivchari/kumo:latest`).
	DefaultKumoImage = "ghcr.io/sivchari/kumo@sha256:e0c98c28ba2de1f873a0a97e918d79cd778b94af2f15329e3ead14c82c7e30ae"

	// KumoEndpoint is kumo's in-cluster endpoint, the value fjord injects
	// as AWS_ENDPOINT_URL into IAM-identity pods so their AWS SDK calls
	// (S3, Secrets Manager, SQS, ...) reach kumo instead of real AWS.
	KumoEndpoint = "http://kumo.kube-system.svc:4566"

	// kumoName is the name of kumo's Deployment and Service.
	kumoName = "kumo"
	// kumoPort is the port kumo listens on and its Service exposes.
	kumoPort = 4566
)

// kumoLabels selects kumo's pods, used as both the Deployment's pod
// template labels and its Service's selector.
var kumoLabels = map[string]string{"app": kumoName}

// EnsureKumo deploys kumo (a LocalStack-compatible AWS emulator covering
// S3, Secrets Manager, SQS, and other services fjord does not itself
// emulate) to the kube-system namespace: its Deployment and ClusterIP
// Service. It is idempotent; repeated calls update the Deployment to run
// image.
func EnsureKumo(ctx context.Context, client kubernetes.Interface, image string) error {
	if err := ensureKumoDeployment(ctx, client, image); err != nil {
		return err
	}

	return ensureKumoService(ctx, client)
}

// ensureKumoDeployment creates kumo's Deployment if it does not already
// exist, or updates it in place (preserving its ResourceVersion) to run
// image otherwise.
func ensureKumoDeployment(ctx context.Context, client kubernetes.Interface, image string) error {
	desired := kumoDeployment(image)

	_, err := client.AppsV1().Deployments(agent.SystemNamespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q deployment: %w", kumoName, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, getErr := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, kumoName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get %q deployment: %w", kumoName, getErr)
		}

		existing.Spec = desired.Spec

		if _, updateErr := client.AppsV1().Deployments(agent.SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("update %q deployment: %w", kumoName, updateErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure %q deployment: %w", kumoName, err)
	}

	return nil
}

// kumoDeployment builds the desired kumo Deployment running image, matching
// the arguments the official image's Docker entrypoint documents (`kumo
// --host 0.0.0.0 --port 4566`).
func kumoDeployment(image string) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kumoName,
			Namespace: agent.SystemNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: kumoLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: kumoLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  kumoName,
							Image: image,
							Args:  []string{"--host", "0.0.0.0", "--port", fmt.Sprintf("%d", kumoPort)},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: kumoPort, Protocol: corev1.ProtocolTCP},
							},
						},
					},
				},
			},
		},
	}
}

// ensureKumoService creates kumo's ClusterIP Service if it does not already
// exist.
func ensureKumoService(ctx context.Context, client kubernetes.Interface) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kumoName,
			Namespace: agent.SystemNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: kumoLabels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: kumoPort, TargetPort: intstr.FromInt32(kumoPort)},
			},
		},
	}

	_, err := client.CoreV1().Services(agent.SystemNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q service: %w", kumoName, err)
	}

	return nil
}
