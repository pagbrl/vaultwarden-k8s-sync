// Command vaultwarden-k8s-sync reads secure-note items from Vaultwarden org
// collections and upserts them as Kubernetes Secrets. It is intended to run as
// an in-cluster CronJob.
//
// One sync run:
//  1. configure + unlock the vault via the `bw` CLI,
//  2. for each configured collection, read every secure note (item type 2),
//  3. parse KEY=VALUE lines from each note body,
//  4. upsert one Kubernetes Secret per mapping (namespace, secretName).
//
// It exits non-zero if any mapping fails. Secret values are never logged.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/pagbrl/vaultwarden-k8s-sync/internal/config"
	"github.com/pagbrl/vaultwarden-k8s-sync/internal/k8s"
	"github.com/pagbrl/vaultwarden-k8s-sync/internal/vaultwarden"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("sync failed", "error", err)
		os.Exit(1)
	}
	logger.Info("sync completed successfully")
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	logger.Info("loaded config",
		"server", cfg.Server,
		"mappings", len(cfg.Mappings),
		"auth", authMode(cfg))

	// --- Vaultwarden ---
	vw := vaultwarden.New(cfg.Server, cfg.Password, cfg.ClientID, cfg.ClientSecret, logger)
	if err := vw.Prepare(ctx); err != nil {
		return err
	}
	defer vw.Lock(ctx)

	// --- Kubernetes ---
	kube, err := k8s.NewInCluster(logger)
	if err != nil {
		return err
	}

	// Reconcile every mapping; collect the first error but attempt them all so
	// one bad collection doesn't block the rest, then fail loudly at the end.
	var firstErr error
	failures := 0
	for _, m := range cfg.Mappings {
		mlog := logger.With("collection", m.CollectionID, "namespace", m.Namespace, "secret", m.SecretName)
		if err := reconcile(ctx, vw, kube, m, mlog); err != nil {
			mlog.Error("mapping failed", "error", err)
			failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}

	if firstErr != nil {
		logger.Error("one or more mappings failed", "failures", failures, "total", len(cfg.Mappings))
		return firstErr
	}
	return nil
}

func reconcile(ctx context.Context, vw *vaultwarden.Client, kube *k8s.Client, m config.Mapping, log *slog.Logger) error {
	if m.PerItem {
		return reconcilePerItem(ctx, vw, kube, m, log)
	}
	notes, err := vw.SecureNotes(ctx, m.CollectionID)
	if err != nil {
		return err
	}
	log.Info("fetched secure notes", "count", len(notes))

	// Merge KEY=VALUE pairs across all notes in the collection. Later notes win
	// on key collisions (bw preserves a stable order per collection).
	data := make(map[string]string)
	for _, body := range notes {
		for k, v := range vaultwarden.ParseNotes(body) {
			data[k] = v
		}
	}
	log.Info("parsed keys", "count", len(data))

	res, err := kube.UpsertSecret(ctx, m.Namespace, m.SecretName, data)
	if err != nil {
		return err
	}

	switch {
	case res.Created:
		log.Info("secret created", "changed_keys", res.Changed)
	case len(res.Changed) > 0 || len(res.Removed) > 0:
		log.Info("secret changed", "changed_keys", res.Changed, "removed_keys", res.Removed)
	default:
		log.Info("secret unchanged")
	}
	return nil
}

// reconcilePerItem upserts one Kubernetes Secret per secure-note item in the
// collection, named after the item (sanitised, optionally prefixed by
// m.SecretName). One bad item does not block the others; the first error is
// returned after attempting all.
func reconcilePerItem(ctx context.Context, vw *vaultwarden.Client, kube *k8s.Client, m config.Mapping, log *slog.Logger) error {
	items, err := vw.SecureNoteItems(ctx, m.CollectionID)
	if err != nil {
		return err
	}
	log.Info("fetched secure-note items", "count", len(items))

	var firstErr error
	for _, it := range items {
		name := sanitizeSecretName(m.SecretName + it.Name)
		data := vaultwarden.ParseNotes(it.Notes)
		ilog := log.With("item", it.Name, "secret", name, "keys", len(data))
		if name == "" {
			ilog.Warn("item name sanitises to empty; skipping")
			continue
		}
		if len(data) == 0 {
			ilog.Warn("item has no KEY=VALUE lines; skipping")
			continue
		}
		res, err := kube.UpsertSecret(ctx, m.Namespace, name, data)
		if err != nil {
			ilog.Error("upsert failed", "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch {
		case res.Created:
			ilog.Info("secret created", "changed_keys", res.Changed)
		case len(res.Changed) > 0 || len(res.Removed) > 0:
			ilog.Info("secret changed", "changed_keys", res.Changed, "removed_keys", res.Removed)
		default:
			ilog.Info("secret unchanged")
		}
	}
	return firstErr
}

var dns1123Invalid = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeSecretName maps an item name to a DNS-1123 label usable as a
// Kubernetes Secret name: lowercased, invalid runs collapsed to '-', trimmed.
func sanitizeSecretName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = dns1123Invalid.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func authMode(cfg *config.Config) string {
	if cfg.HasAPIKey() {
		return "apikey+password"
	}
	return "password"
}
