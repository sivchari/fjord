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
	// localPathProvisioner is the provisioner kind's bundled local-path
	// storage uses; fjord's gp2 class reuses it as the local stand-in for
	// EBS-backed gp2.
	localPathProvisioner = "rancher.io/local-path"
	// kindDefaultStorageClass is the StorageClass kind installs by default.
	kindDefaultStorageClass = "standard"
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
// (both backed by kind's local-path provisioner), with gp2 as the cluster
// default, and demotes kind's standard class so exactly one default remains,
// matching a new EKS cluster. gp3 is not a default on EKS but is widely used
// (real EKS workloads commonly set storageClassName: gp3), so providing it
// lets those PVCs bind unchanged. kind installs its default StorageClass
// before cluster creation returns, so no waiting is required here.
func EnsureDefaultStorageClass(ctx context.Context, client kubernetes.Interface) error {
	if err := demoteStandardClass(ctx, client); err != nil {
		return err
	}

	if err := createStorageClass(ctx, client, "gp2", true); err != nil {
		return err
	}

	return createStorageClass(ctx, client, "gp3", false)
}

// demoteStandardClass removes the default-class annotation from kind's
// standard StorageClass. A missing class is not an error.
func demoteStandardClass(ctx context.Context, client kubernetes.Interface) error {
	standard, err := client.StorageV1().StorageClasses().Get(ctx, kindDefaultStorageClass, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get %q storage class: %w", kindDefaultStorageClass, err)
	}

	if standard.Annotations[defaultClassAnnotation] != defaultClassValue {
		return nil
	}

	standard.Annotations[defaultClassAnnotation] = "false"

	if _, err := client.StorageV1().StorageClasses().Update(ctx, standard, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("demote %q storage class: %w", kindDefaultStorageClass, err)
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
