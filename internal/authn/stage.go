// Package authn stages what fjord's control-plane authentication token
// webhook needs before a cluster is created: the webhook kubeconfig
// kube-apiserver calls, and the authenticator's TLS serving certificate.
// Stage writes the webhook kubeconfig under a host directory;
// internal/provider.Config.ExtraMounts delivers it into the node before the
// API server starts, and internal/provider.Config.AuthWebhook configures
// kubeadm to call the authenticator. The TLS certificate is not written to
// that directory: unlike the webhook kubeconfig, it is not needed on the
// node itself, only by the authenticator pod, which cluster.EnsureAuthenticator
// delivers it to via a Secret built from StagedAuthn.Cert. The authenticator
// itself runs as a DaemonSet (cluster.EnsureAuthenticator), created after
// the cluster comes up.
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
	// webhook kubeconfig.
	authnDirName = "authn"

	// nodeAuthnDir is where the authn subdirectory is delivered inside the
	// cluster, matching provider.AuthWebhook.VolumeMountPath and the
	// ConfigFilePath's parent directory.
	nodeAuthnDir = "/etc/fjord/authn"

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
)

// StagedAuthn is the result of Stage: the host directory holding the staged
// webhook kubeconfig, the provider.Mounts delivering it to the control-plane
// node, the provider.AuthWebhook configuring kubeadm to call the
// authenticator, and the authenticator's TLS serving certificate.
type StagedAuthn struct {
	// Dir is the host directory holding webhook.yaml (baseDir/authn).
	Dir string
	// Mounts are the provider.Mounts delivering Dir to the control-plane
	// node.
	Mounts []provider.Mount
	// Webhook configures kubeadm's apiserver authentication token webhook
	// to call the authenticator via the mounted webhook.yaml.
	Webhook *provider.AuthWebhook
	// Cert is the authenticator's TLS serving certificate, issued from ca.
	// It is not staged to disk: cluster.EnsureAuthenticator stores it in a
	// Secret the authenticator DaemonSet mounts.
	Cert *pki.ServerCert
}

// Stage writes the webhook kubeconfig under baseDir and issues the
// authenticator's TLS serving certificate, returning the provider.Mounts and
// provider.AuthWebhook wiring the former into a cluster not yet created,
// alongside the latter for cluster.EnsureAuthenticator to deliver via a
// Secret. ca issues the certificate (the same CA the webhook kubeconfig
// trusts).
func Stage(baseDir string, ca *pki.CA) (*StagedAuthn, error) {
	dir := filepath.Join(baseDir, authnDirName)
	if err := os.MkdirAll(dir, authnDirMode); err != nil {
		return nil, fmt.Errorf("create authn staging directory %q: %w", dir, err)
	}

	cert, err := issueAuthenticatorCert(ca)
	if err != nil {
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
			VolumeMountPath: nodeAuthnDir,
		},
		Cert: cert,
	}, nil
}

// Cleanup removes the directory tree Stage created under baseDir (the
// parent of s.Dir). Callers must only call this once the cluster it was
// staged for no longer exists: the webhook kubeconfig must remain in place
// on the host for the lifetime of the control-plane node's bind mounts.
func (s *StagedAuthn) Cleanup() error {
	if err := os.RemoveAll(filepath.Dir(s.Dir)); err != nil {
		return fmt.Errorf("remove authn staging directory: %w", err)
	}

	return nil
}

// issueAuthenticatorCert issues the authenticator's TLS serving certificate
// from ca, valid for authenticatorServerName, the hostname the webhook
// kubeconfig dials.
func issueAuthenticatorCert(ca *pki.CA) (*pki.ServerCert, error) {
	cert, err := ca.IssueServerCert(cluster.AuthenticatorServiceAccountName, []string{authenticatorServerName})
	if err != nil {
		return nil, fmt.Errorf("issue authenticator server certificate: %w", err)
	}

	return cert, nil
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
