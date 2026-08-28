// Package k8s provides an in-cluster client that upserts Kubernetes Secrets.
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

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

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return c.create(ctx, namespace, name, data)
	}
	if err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}

	return c.update(ctx, existing, data)
}

func (c *Client) create(ctx context.Context, namespace, name string, data map[string]string) (*ApplyResult, error) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedByLabelKey: ManagedByLabelValue,
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}

	if _, err := c.cs.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating secret %s/%s: %w", namespace, name, err)
	}

	res := &ApplyResult{Created: true, Changed: sortedKeys(data)}
	c.log.Info("created secret",
		"namespace", namespace, "name", name, "keys", res.Changed)
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
