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
	// defaultClassAnnotation marks a StorageClass as the cluster default.
	defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
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

// EnsureDefaultStorageClass creates the EKS-style gp2 StorageClass (backed by
// kind's local-path provisioner) as the cluster default and demotes kind's
// standard class so exactly one default remains, matching a new EKS cluster.
// kind installs its default StorageClass before cluster creation returns, so
// no waiting is required here.
func EnsureDefaultStorageClass(ctx context.Context, client kubernetes.Interface) error {
	if err := demoteStandardClass(ctx, client); err != nil {
		return err
	}

	return createGP2Class(ctx, client)
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

	if standard.Annotations[defaultClassAnnotation] != "true" {
		return nil
	}

	standard.Annotations[defaultClassAnnotation] = "false"

	if _, err := client.StorageV1().StorageClasses().Update(ctx, standard, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("demote %q storage class: %w", kindDefaultStorageClass, err)
	}

	return nil
}

// createGP2Class creates the gp2 StorageClass if it does not already exist.
func createGP2Class(ctx context.Context, client kubernetes.Interface) error {
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer

	gp2 := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gp2",
			Annotations: map[string]string{defaultClassAnnotation: "true"},
		},
		Provisioner:       localPathProvisioner,
		VolumeBindingMode: &bindingMode,
	}

	_, err := client.StorageV1().StorageClasses().Create(ctx, gp2, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create gp2 storage class: %w", err)
	}

	return nil
}
