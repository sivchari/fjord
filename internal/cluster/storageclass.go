// Package cluster adjusts a freshly created cluster to match the default
// state of a new Amazon EKS cluster.
package cluster

import (
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// defaultClassAnnotation marks a StorageClass as the cluster default, and
	// defaultClassValue is its truthy value.
	defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	defaultClassValue      = "true"
	// localPathProvisioner is the provisioner rask's bundled local-path
	// storage uses; fjord's gp2 class reuses it as the local stand-in for
	// EBS-backed gp2.
	localPathProvisioner = "rancher.io/local-path"
)

// NewClient builds a Kubernetes client from raw kubeconfig content.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	return client, nil
}

// EnsureDefaultStorageClass creates the EKS-style gp2 and gp3 StorageClasses
// (both backed by rask's bundled local-path provisioner), with gp2 as the
// cluster default, and demotes every other default-annotated class so
// exactly one default remains, matching a new EKS cluster. rask's bundle
// marks its own local-path class as the default (and clusters may also
// carry a standard alias), so demotion goes by the annotation, not by
// name. gp3 is not a default on EKS but is widely used (real EKS
// workloads commonly set storageClassName: gp3), so providing it lets
// those PVCs bind unchanged. rask installs its default StorageClasses
// before cluster creation returns, so no waiting is required here.
func EnsureDefaultStorageClass(ctx context.Context, client kubernetes.Interface) error {
	if err := demoteDefaultClasses(ctx, client); err != nil {
		return err
	}

	if err := createStorageClass(ctx, client, "gp2", true); err != nil {
		return err
	}

	return createStorageClass(ctx, client, "gp3", false)
}

// demoteDefaultClasses removes the default-class annotation from every
// StorageClass that carries it, except gp2 (which EnsureDefaultStorageClass
// is about to make, or keep, the sole default).
func demoteDefaultClasses(ctx context.Context, client kubernetes.Interface) error {
	classes, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}

	for i := range classes.Items {
		sc := &classes.Items[i]
		if sc.Name == "gp2" || sc.Annotations[defaultClassAnnotation] != defaultClassValue {
			continue
		}

		sc.Annotations[defaultClassAnnotation] = "false"

		if _, err := client.StorageV1().StorageClasses().Update(ctx, sc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("demote %q storage class: %w", sc.Name, err)
		}
	}

	return nil
}

// createStorageClass creates a StorageClass named name backed by the
// local-path provisioner, marked as the cluster default when isDefault is set,
// if it does not already exist.
func createStorageClass(ctx context.Context, client kubernetes.Interface, name string, isDefault bool) error {
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer

	var annotations map[string]string
	if isDefault {
		annotations = map[string]string{defaultClassAnnotation: defaultClassValue}
	}

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Provisioner:       localPathProvisioner,
		VolumeBindingMode: &bindingMode,
	}

	_, err := client.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create %q storage class: %w", name, err)
	}

	return nil
}
