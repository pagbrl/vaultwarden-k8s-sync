package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The reason this file exists: an `imagePullSecrets` entry is ignored by
// kubelet unless the Secret's type is kubernetes.io/dockerconfigjson, and the
// type is set from a reserved key in the vault note.
func TestUpsertSecretHonoursTypeKey(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewWithClientset(cs, discard())

	const dockercfg = `{"auths":{"ghcr.io":{"auth":"dXNlcjp0b2tlbg=="}}}`

	if _, err := c.UpsertSecret(context.Background(), "staging", "ghcr", map[string]string{
		TypeKey:             "kubernetes.io/dockerconfigjson",
		".dockerconfigjson": dockercfg,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := cs.CoreV1().Secrets("staging").Get(context.Background(), "ghcr", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %q, want kubernetes.io/dockerconfigjson", got.Type)
	}
	// The reserved key must not leak into the data: a dockerconfigjson Secret
	// carrying a stray K8S_SECRET_TYPE entry is rejected by the API server.
	if _, ok := got.StringData[TypeKey]; ok {
		t.Errorf("reserved key %s leaked into the secret data", TypeKey)
	}
	if got.StringData[".dockerconfigjson"] != dockercfg {
		t.Errorf("dockerconfigjson value not transported verbatim")
	}
}

// No type key at all: every item written before this feature existed must keep
// producing exactly what it produced.
func TestUpsertSecretDefaultsToOpaque(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewWithClientset(cs, discard())

	if _, err := c.UpsertSecret(context.Background(), "staging", "outline-env", map[string]string{
		"DATABASE_URL": "postgres://x",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, _ := cs.CoreV1().Secrets("staging").Get(context.Background(), "outline-env", metav1.GetOptions{})
	if got.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q, want Opaque", got.Type)
	}
}

// A type change cannot be applied (the field is immutable) and this tool has no
// `delete` verb, so it must fail with an actionable message rather than write
// nothing and report success.
func TestUpsertSecretRefusesTypeChange(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ghcr", Namespace: "staging"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{".dockerconfigjson": []byte("{}")},
	})
	c := NewWithClientset(cs, discard())

	_, err := c.UpsertSecret(context.Background(), "staging", "ghcr", map[string]string{
		TypeKey:             "kubernetes.io/dockerconfigjson",
		".dockerconfigjson": "{}",
	})
	if err == nil {
		t.Fatal("expected an error when the existing Secret has another type")
	}
}
