// Package k8s provides an in-cluster client that upserts Kubernetes Secrets.
package k8s

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ManagedByLabel marks Secrets owned by this tool.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "vaultwarden-k8s-sync"

	// TypeKey is a RESERVED key: a note that carries it sets the Kubernetes
	// Secret's `type` instead of contributing a data entry. It never reaches
	// the Secret's data.
	//
	// Why a key in the note body and not a field on the Mapping: a mapping
	// binds a whole collection, and a collection holds ordinary `<app>-env`
	// notes next to the special one. The type is a property of the item, so
	// it is declared by the item.
	//
	// The only type that needs this today is `kubernetes.io/dockerconfigjson`,
	// which is what `imagePullSecrets` requires — kubelet ignores an Opaque
	// Secret even when it carries a correct `.dockerconfigjson` key. Such a
	// note reads:
	//
	//	K8S_SECRET_TYPE=kubernetes.io/dockerconfigjson
	//	.dockerconfigjson={"auths":{"ghcr.io":{"auth":"<base64 user:token>"}}}
	//
	// No allow-list here on purpose: the API server already validates that a
	// typed Secret carries the keys its type requires, and it rejects an
	// unknown builtin type. Duplicating that list would only make this tool
	// lag behind Kubernetes.
	TypeKey = "K8S_SECRET_TYPE"

	// B64Suffix marks a value carried base64-encoded: `<key>.b64=<base64>` in
	// the note lands as `<key>` in the Secret, decoded. A note is parsed line
	// by line, so this is the only way a multi-line value — an SSH private
	// key, a PEM certificate — gets through it.
	B64Suffix = ".b64"
)

// Client wraps a Kubernetes clientset.
type Client struct {
	cs  kubernetes.Interface
	log *slog.Logger
}

// NewInCluster builds a Client using the in-cluster service-account config.
func NewInCluster(log *slog.Logger) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return &Client{cs: cs, log: log}, nil
}

// NewWithClientset builds a Client from an existing clientset (useful for tests).
func NewWithClientset(cs kubernetes.Interface, log *slog.Logger) *Client {
	return &Client{cs: cs, log: log}
}

// ApplyResult reports what changed during an upsert. Values are never included.
type ApplyResult struct {
	Created bool
	// Changed lists the keys whose values were added or modified.
	Changed []string
	// Removed lists keys that existed before but are no longer present.
	Removed []string
}

// UpsertSecret creates or updates an Opaque Secret in namespace with the given
// data (as StringData). The operation is idempotent: if the desired data equals
// the existing data, no write is performed.
//
// It returns an ApplyResult describing which keys changed. Secret *values* are
// never logged or returned.
func (c *Client) UpsertSecret(ctx context.Context, namespace, name string, data map[string]string) (*ApplyResult, error) {
	secrets := c.cs.CoreV1().Secrets(namespace)

	data, secretType := splitType(data)
	data, err := decodeB64(data)
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, err)
	}

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return c.create(ctx, namespace, name, data, secretType)
	}
	if err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}

	// A Secret's `type` is IMMUTABLE: the API server rejects an update that
	// changes it. Recreating it here is not an option either — this tool's
	// Role deliberately has no `delete` verb (see deploy/rbac.yaml), and
	// deleting a Secret out from under running pods is not something a
	// 15-minute CronJob should decide on its own.
	//
	// So we stop, loudly, and name the one command that fixes it. Silently
	// leaving the old type would be worse: an `imagePullSecrets` pointing at a
	// Secret that stayed Opaque fails at image pull, far from here.
	if existing.Type != secretType {
		return nil, fmt.Errorf(
			"secret %s/%s has type %q but the vault item asks for %q, and type is immutable; "+
				"delete it once (kubectl -n %s delete secret %s) and the next run recreates it",
			namespace, name, existing.Type, secretType, namespace, name)
	}

	return c.update(ctx, existing, data)
}

// splitType pops the reserved TypeKey out of the parsed note data and returns
// the remaining data plus the Secret type to use. Absent the key, the type is
// Opaque — which is what every item produced before this existed.
//
// The input map is not mutated: it belongs to the caller, which logs its keys.
func splitType(data map[string]string) (map[string]string, corev1.SecretType) {
	raw, ok := data[TypeKey]
	if !ok {
		return data, corev1.SecretTypeOpaque
	}

	out := make(map[string]string, len(data)-1)
	for k, v := range data {
		if k != TypeKey {
			out[k] = v
		}
	}

	t := strings.TrimSpace(raw)
	if t == "" {
		return out, corev1.SecretTypeOpaque
	}
	return out, corev1.SecretType(t)
}

// decodeB64 replaces every `<key>.b64` entry with `<key>` and its decoded
// value. The input map is not mutated, for the same reason as splitType.
func decodeB64(data map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(data))
	for k, v := range data {
		name, ok := strings.CutSuffix(k, B64Suffix)
		if !ok {
			out[k] = v
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			// The key is named, never the value.
			return nil, fmt.Errorf("key %s is not valid base64: %w", k, err)
		}
		out[name] = string(raw)
	}
	return out, nil
}

func (c *Client) create(ctx context.Context, namespace, name string, data map[string]string, secretType corev1.SecretType) (*ApplyResult, error) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedByLabelKey: ManagedByLabelValue,
			},
		},
		Type:       secretType,
		StringData: data,
	}

	if _, err := c.cs.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating secret %s/%s: %w", namespace, name, err)
	}

	res := &ApplyResult{Created: true, Changed: sortedKeys(data)}
	c.log.Info("created secret",
		"namespace", namespace, "name", name, "type", string(secretType), "keys", res.Changed)
	return res, nil
}

func (c *Client) update(ctx context.Context, existing *corev1.Secret, data map[string]string) (*ApplyResult, error) {
	namespace, name := existing.Namespace, existing.Name

	// Compare desired (data) against current (existing.Data, which is []byte).
	changed, removed := diff(existing.Data, data)

	labelsOK := existing.Labels[ManagedByLabelKey] == ManagedByLabelValue

	if len(changed) == 0 && len(removed) == 0 && labelsOK {
		c.log.Info("secret already up to date", "namespace", namespace, "name", name)
		return &ApplyResult{}, nil
	}

	updated := existing.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels[ManagedByLabelKey] = ManagedByLabelValue

	// Replace the managed data wholesale so removed keys disappear. We set
	// StringData (server merges into Data) and clear Data to avoid stale keys.
	updated.Data = nil
	updated.StringData = data

	if _, err := c.cs.CoreV1().Secrets(namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("updating secret %s/%s: %w", namespace, name, err)
	}

	res := &ApplyResult{Changed: changed, Removed: removed}
	c.log.Info("updated secret",
		"namespace", namespace, "name", name,
		"changed_keys", changed, "removed_keys", removed)
	return res, nil
}

// diff compares current secret data (bytes) with desired (strings) and returns
// the keys that were added/modified (changed) and keys removed.
func diff(current map[string][]byte, desired map[string]string) (changed, removed []string) {
	for k, v := range desired {
		cur, ok := current[k]
		if !ok || string(cur) != v {
			changed = append(changed, k)
		}
	}
	for k := range current {
		if _, ok := desired[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(changed)
	sort.Strings(removed)
	return changed, removed
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
