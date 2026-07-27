// Package pki generates the self-signed CA and server certificates fjord
// uses to secure its admission webhooks (pod-identity-webhook, the fjord
// injector webhook, and the authenticator webhook). It does not depend on
// cert-manager: fjord creates the CA itself, stores the resulting
// certificate/key pair in a Secret, and embeds the CA certificate in the
// relevant MutatingWebhookConfiguration's caBundle.
package pki
