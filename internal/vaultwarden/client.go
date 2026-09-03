// Package vaultwarden is a thin wrapper around the official Bitwarden CLI (`bw`)
// that talks to a self-hosted Vaultwarden server.
//
// Vaultwarden implements the Bitwarden *password-manager* API, not the
// Bitwarden *Secrets Manager* API. That is why the External Secrets Operator
// Bitwarden provider (which speaks Secrets Manager) cannot read from it. The
// `bw` CLI, however, speaks the password-manager API, so we shell out to it.
package vaultwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Item is the subset of a `bw list items` element we care about.
type Item struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  int    `json:"type"`
	Notes string `json:"notes"`
}

const secureNoteType = 2 // Bitwarden item type for "Secure Note".

// Client drives the `bw` CLI. It is not safe for concurrent use.
type Client struct {
	// BinPath is the path to the bw executable (default "bw").
	BinPath string
	// Server is the Vaultwarden base URL.
	Server string
	// Password unlocks the vault (used via BW_PASSWORD env for `bw unlock`).
	Password string
	// ClientID / ClientSecret enable `bw login --apikey` when not logged in.
	ClientID     string
	ClientSecret string

	log     *slog.Logger
	session string
}

// New constructs a Client.
func New(server, password, clientID, clientSecret string, log *slog.Logger) *Client {
	bin := os.Getenv("BW_CLI")
	if bin == "" {
		bin = "bw"
	}
	return &Client{
		BinPath:      bin,
		Server:       server,
		Password:     password,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		log:          log,
	}
}

// Prepare configures the server, ensures the CLI is authenticated, unlocks the
// vault to obtain a session token, and runs an initial sync.
func (c *Client) Prepare(ctx context.Context) error {
	if err := c.configureServer(ctx); err != nil {
		return err
	}
	if err := c.ensureLoggedIn(ctx); err != nil {
		return err
	}
	if err := c.unlock(ctx); err != nil {
		return err
	}
	if err := c.Sync(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Client) configureServer(ctx context.Context) error {
	c.log.Info("configuring vaultwarden server", "server", c.Server)
	_, err := c.run(ctx, nil, "config", "server", c.Server)
	if err != nil {
		return fmt.Errorf("bw config server: %w", err)
	}
	return nil
}

// ensureLoggedIn logs in with API-key credentials if provided and the CLI is
// not already authenticated. If no API key is provided we assume a prior
// `bw login` has established an account (unlock will fail loudly otherwise).
func (c *Client) ensureLoggedIn(ctx context.Context) error {
	status, err := c.status(ctx)
	if err != nil {
		return err
	}
	if status != "unauthenticated" {
		c.log.Info("bw already authenticated", "status", status)
		return nil
	}

	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("bw is unauthenticated and no BW_CLIENTID/BW_CLIENTSECRET provided to log in")
	}

	c.log.Info("logging in with API key")
	env := []string{
		"BW_CLIENTID=" + c.ClientID,
		"BW_CLIENTSECRET=" + c.ClientSecret,
	}
	if _, err := c.run(ctx, env, "login", "--apikey"); err != nil {
		return fmt.Errorf("bw login --apikey: %w", err)
	}
	return nil
}

// status returns the CLI status string: unauthenticated | locked | unlocked.
func (c *Client) status(ctx context.Context) (string, error) {
	out, err := c.run(ctx, nil, "status")
	if err != nil {
		return "", fmt.Errorf("bw status: %w", err)
	}
	var s struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return "", fmt.Errorf("parsing bw status: %w", err)
	}
	return s.Status, nil
}

// unlock unlocks the vault and stores the resulting session token.
func (c *Client) unlock(ctx context.Context) error {
	c.log.Info("unlocking vault")
	env := []string{"BW_PASSWORD=" + c.Password}
	out, err := c.run(ctx, env, "unlock", "--passwordenv", "BW_PASSWORD", "--raw")
	if err != nil {
		return fmt.Errorf("bw unlock: %w", err)
	}
	session := strings.TrimSpace(string(out))
	if session == "" {
		return fmt.Errorf("bw unlock returned an empty session token")
	}
	c.session = session
	return nil
}

// Sync pulls the latest vault state from the server.
func (c *Client) Sync(ctx context.Context) error {
	c.log.Info("syncing vault")
	if _, err := c.run(ctx, c.sessionEnv(), "sync"); err != nil {
		return fmt.Errorf("bw sync: %w", err)
	}
	return nil
}

// SecureNotes returns the raw `.notes` body of every secure note (type 2) in
// the given collection.
func (c *Client) SecureNotes(ctx context.Context, collectionID string) ([]string, error) {
	out, err := c.run(ctx, c.sessionEnv(),
		"list", "items", "--collectionid", collectionID, "--session", c.session)
	if err != nil {
		return nil, fmt.Errorf("bw list items (collection %s): %w", collectionID, err)
	}

	var items []Item
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parsing bw list items output: %w", err)
	}

	var notes []string
	for _, it := range items {
		if it.Type != secureNoteType {
			continue
		}
		if strings.TrimSpace(it.Notes) == "" {
			c.log.Warn("secure note has empty body", "item", it.Name, "id", it.ID)
			continue
		}
		notes = append(notes, it.Notes)
	}
	return notes, nil
}

// Lock locks the vault, discarding the in-memory session. Best-effort.
func (c *Client) Lock(ctx context.Context) {
	if c.session == "" {
		return
	}
	if _, err := c.run(ctx, nil, "lock"); err != nil {
		c.log.Warn("bw lock failed", "error", err)
	}
	c.session = ""
}

func (c *Client) sessionEnv() []string {
	if c.session == "" {
		return nil
	}
	return []string{"BW_SESSION=" + c.session}
}

// run executes the bw CLI. extraEnv entries (KEY=VALUE) are appended to the
// inherited environment. Secret values passed via env are never logged.
func (c *Client) run(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.BinPath, args...)
	cmd.Env = append(os.Environ(), extraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// stderr may contain diagnostic text; it must not contain secret values
		// for the commands we invoke, but we still keep it terse.
		return nil, fmt.Errorf("%s %s: %w: %s",
			c.BinPath, safeArgs(args), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// safeArgs renders args for logging (args never contain secrets in this client;
// secrets are always passed via env), joining them for readability.
func safeArgs(args []string) string {
	return strings.Join(args, " ")
}
