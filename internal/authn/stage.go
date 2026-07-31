// Package authn stages the host-side files fjord's control-plane
// authentication token webhook needs before a cluster is created: the
// authenticator's TLS serving certificate and the webhook kubeconfig
// kube-apiserver calls. Stage writes these under a host directory;
// internal/provider.Config.ExtraMounts delivers them into the node before
// the API server starts, and internal/provider.Config.AuthWebhook
// configures kubeadm to call the authenticator. The authenticator itself
// runs as a DaemonSet (cluster.EnsureAuthenticator), created after the
// cluster comes up.
package authn

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/pki"
	"github.com/sivchari/fjord/internal/provider"
)

const (
	// authnDirName is the subdirectory of baseDir (see Stage) holding the
	// authenticator's TLS material and webhook kubeconfig.
	authnDirName = "authn"

	// nodeAuthnDir is where the authn subdirectory is mounted inside the
	// control-plane node, matching provider.AuthWebhook.VolumeHostPath /
	// VolumeMountPath and the ConfigFilePath's parent directory. The
	// authenticator DaemonSet also hostPath-mounts it for its TLS material.
	nodeAuthnDir = "/etc/fjord/authn"

	tlsCertFileName = "tls.crt"
	tlsKeyFileName  = "tls.key"
	webhookFileName = "webhook.yaml"

	// authenticatorPort is the port the authenticator listens on, reachable
	// from kube-apiserver at authenticatorServerName because both run
	// hostNetwork in the same network namespace.
	authenticatorPort = 9443

	// authenticatorServerName is the hostname the webhook kubeconfig
	// dials and the authenticator's TLS certificate is issued for.
	// "localhost" is used rather than the literal IP "127.0.0.1": Go's TLS
	// client only checks a certificate's IP SANs against an IP-literal
	// ServerName, never its DNSNames, and pki.CA.IssueServerCert only
	// issues DNSName SANs. Every container's /etc/hosts maps "localhost"
	// to 127.0.0.1, so this resolves identically within the hostNetwork
	// netns while letting the certificate carry an ordinary DNS SAN.
	authenticatorServerName = "localhost"

	// authnKubeconfigName names the single cluster/context/user entry in
	// the webhook kubeconfig; the file has exactly one of each.
	authnKubeconfigName = "fjord-authenticator"

	authnDirMode = 0o755
	tlsCertMode  = 0o644
	tlsKeyMode   = 0o600
)

// StagedAuthn is the result of Stage: the host directory holding the
// authenticator's staged files, the provider.Mounts delivering them to the
// control-plane node, and the provider.AuthWebhook configuring kubeadm to
// call the authenticator.
type StagedAuthn struct {
	// Dir is the host directory holding tls.crt, tls.key, and
	// webhook.yaml (baseDir/authn).
	Dir string
	// Mounts are the provider.Mounts delivering Dir to the control-plane
	// node.
	Mounts []provider.Mount
	// Webhook configures kubeadm's apiserver authentication token webhook
	// to call the authenticator via the mounted webhook.yaml.
	Webhook *provider.AuthWebhook
}

// Stage writes the authenticator's TLS certificate and webhook kubeconfig
// under baseDir, and returns the provider.Mounts and provider.AuthWebhook
// wiring them into a cluster not yet created. ca issues the authenticator's
// serving certificate (the same CA the webhook kubeconfig trusts).
func Stage(baseDir string, ca *pki.CA) (*StagedAuthn, error) {
	dir := filepath.Join(baseDir, authnDirName)
	if err := os.MkdirAll(dir, authnDirMode); err != nil {
		return nil, fmt.Errorf("create authn staging directory %q: %w", dir, err)
	}

	if err := stageTLS(dir, ca); err != nil {
		return nil, err
	}

	if err := stageWebhookKubeconfig(dir, ca); err != nil {
		return nil, err
	}

	return &StagedAuthn{
		Dir: dir,
		Mounts: []provider.Mount{
			{HostPath: dir, ContainerPath: nodeAuthnDir},
		},
		Webhook: &provider.AuthWebhook{
			ConfigFilePath:  nodeAuthnDir + "/" + webhookFileName,
			VolumeName:      "fjord-authn",
			VolumeHostPath:  nodeAuthnDir,
			VolumeMountPath: nodeAuthnDir,
		},
	}, nil
}

// Cleanup removes the directory tree Stage created under baseDir (the
// parent of s.Dir). Callers must only call this once the cluster it was
// staged for no longer exists: the TLS material must remain in place on the
// host for the lifetime of the control-plane node's bind mounts.
func (s *StagedAuthn) Cleanup() error {
	if err := os.RemoveAll(filepath.Dir(s.Dir)); err != nil {
		return fmt.Errorf("remove authn staging directory: %w", err)
	}

	return nil
}

// stageTLS issues the authenticator's TLS serving certificate from ca and
// writes it to dir/tls.crt and dir/tls.key.
func stageTLS(dir string, ca *pki.CA) error {
	cert, err := ca.IssueServerCert(cluster.AuthenticatorServiceAccountName, []string{authenticatorServerName})
	if err != nil {
		return fmt.Errorf("issue authenticator server certificate: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, tlsCertFileName), cert.CertPEM, tlsCertMode); err != nil {
		return fmt.Errorf("write authenticator tls certificate: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, tlsKeyFileName), cert.KeyPEM, tlsKeyMode); err != nil {
		return fmt.Errorf("write authenticator tls key: %w", err)
	}

	return nil
}

// stageWebhookKubeconfig writes the kube-apiserver
// authentication-token-webhook-config-file kubeconfig to dir/webhook.yaml,
// pointing it at the authenticator's TokenReview endpoint (served at the
// request root; see agent.Authenticator.Handler) and trusting ca.
func stageWebhookKubeconfig(dir string, ca *pki.CA) error {
	server := fmt.Sprintf("https://%s:%d/", authenticatorServerName, authenticatorPort)

	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			authnKubeconfigName: {
				Server:                   server,
				CertificateAuthorityData: ca.CertPEM,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			authnKubeconfigName: {Cluster: authnKubeconfigName, AuthInfo: authnKubeconfigName},
		},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{authnKubeconfigName: {}},
		CurrentContext: authnKubeconfigName,
	}

	if err := clientcmd.WriteToFile(config, filepath.Join(dir, webhookFileName)); err != nil {
		return fmt.Errorf("write webhook kubeconfig: %w", err)
	}

	return nil
}
