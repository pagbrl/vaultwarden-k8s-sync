// Package config loads runtime configuration from environment variables and
// command-line flags for the vaultwarden-k8s-sync tool.
//
// The tool is designed to run as an in-cluster Kubernetes CronJob. The
// CronJob's schedule controls *how often* the sync runs; there is no internal
// loop/interval. If you want to run it as a long-lived Deployment instead, wrap
// the sync in your own ticker using SYNC_INTERVAL as guidance — but the
// recommended and supported deployment model is a CronJob (see deploy/).
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Mapping describes one Vaultwarden collection -> Kubernetes Secret binding.
//
// Every secure note (item type 2) in CollectionID has its `.notes` body parsed
// for KEY=VALUE lines; the union of all parsed keys becomes the data of a single
// Kubernetes Secret named SecretName in Namespace.
type Mapping struct {
	CollectionID string `json:"collectionId"`
	Namespace    string `json:"namespace"`
	SecretName   string `json:"secretName"`

	// PerItem, when true, upserts ONE Kubernetes Secret per secure-note item in
	// the collection — named after the item (sanitised to a DNS-1123 label),
	// with that item's own KEY=VALUE lines as its data — instead of merging all
	// items into a single SecretName. In per-item mode SecretName is optional and
	// used as a name prefix (e.g. prefix "app-" + item "backend-env" =>
	// "app-backend-env"); empty prefix keeps the item name as-is.
	PerItem bool `json:"perItem"`
}

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Server is the base URL of the self-hosted Vaultwarden instance, e.g.
	// https://vaultwarden.services.osmose.co
	Server string

	// Password is the master password used to unlock the vault (BW_PASSWORD).
	// Passed to `bw unlock --passwordenv`. Required.
	Password string

	// ClientID / ClientSecret are optional API-key credentials used for
	// `bw login --apikey` when the CLI is not already authenticated.
	// (BW_CLIENTID / BW_CLIENTSECRET.)
	ClientID     string
	ClientSecret string

	// Mappings is the list of collection -> Secret bindings to reconcile.
	Mappings []Mapping

	// SyncInterval is advisory only (see package doc). Empty means "driven by
	// the CronJob schedule".
	SyncInterval string
}

// Load resolves configuration from flags and environment variables.
//
// Precedence: an explicitly-set flag wins over the corresponding environment
// variable. The collection mappings can be supplied either as a JSON array in
// SYNC_MAPPINGS / --mappings, or as one or more repeated
// --map collectionID:namespace:secretName flags.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("vaultwarden-k8s-sync", flag.ContinueOnError)

	var (
		server       = fs.String("server", os.Getenv("VAULTWARDEN_SERVER"), "Vaultwarden base URL (env VAULTWARDEN_SERVER)")
		password     = fs.String("password", os.Getenv("BW_PASSWORD"), "Vault master password (env BW_PASSWORD)")
		clientID     = fs.String("client-id", os.Getenv("BW_CLIENTID"), "Bitwarden API-key client_id (env BW_CLIENTID)")
		clientSecret = fs.String("client-secret", os.Getenv("BW_CLIENTSECRET"), "Bitwarden API-key client_secret (env BW_CLIENTSECRET)")
		mappingsJSON = fs.String("mappings", os.Getenv("SYNC_MAPPINGS"), "JSON array of {collectionId,namespace,secretName} (env SYNC_MAPPINGS)")
		syncInterval = fs.String("sync-interval", os.Getenv("SYNC_INTERVAL"), "Advisory interval; the CronJob schedule is authoritative (env SYNC_INTERVAL)")
	)

	var repeated multiFlag
	fs.Var(&repeated, "map", "Mapping as collectionID:namespace:secretName (repeatable)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &Config{
		Server:       strings.TrimSpace(*server),
		Password:     *password,
		ClientID:     strings.TrimSpace(*clientID),
		ClientSecret: *clientSecret,
		SyncInterval: strings.TrimSpace(*syncInterval),
	}

	mappings, err := resolveMappings(*mappingsJSON, repeated)
	if err != nil {
		return nil, err
	}
	cfg.Mappings = mappings

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolveMappings(jsonStr string, repeated multiFlag) ([]Mapping, error) {
	var out []Mapping

	if s := strings.TrimSpace(jsonStr); s != "" {
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("parsing SYNC_MAPPINGS json: %w", err)
		}
	}

	for _, raw := range repeated {
		parts := strings.SplitN(raw, ":", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid --map %q: want collectionID:namespace:secretName", raw)
		}
		out = append(out, Mapping{
			CollectionID: strings.TrimSpace(parts[0]),
			Namespace:    strings.TrimSpace(parts[1]),
			SecretName:   strings.TrimSpace(parts[2]),
		})
	}

	return out, nil
}

func (c *Config) validate() error {
	if c.Server == "" {
		return fmt.Errorf("VAULTWARDEN_SERVER is required")
	}
	if c.Password == "" {
		return fmt.Errorf("BW_PASSWORD is required to unlock the vault")
	}
	if len(c.Mappings) == 0 {
		return fmt.Errorf("at least one collection->secret mapping is required (SYNC_MAPPINGS or --map)")
	}
	for i, m := range c.Mappings {
		if m.CollectionID == "" || m.Namespace == "" {
			return fmt.Errorf("mapping[%d] needs collectionId and namespace: %+v", i, m)
		}
		// In per-item mode SecretName is an optional prefix; in merge mode it is
		// the (required) single Secret name.
		if !m.PerItem && m.SecretName == "" {
			return fmt.Errorf("mapping[%d] needs secretName (or set perItem:true): %+v", i, m)
		}
	}
	return nil
}

// HasAPIKey reports whether API-key credentials were provided.
func (c *Config) HasAPIKey() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// multiFlag collects repeated string flags.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
