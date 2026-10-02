package vaultwarden

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBW writes a stub `bw` that answers `status` with the given serverUrl and
// appends every invocation to a log file, so a test can assert which
// subcommands ran.
func fakeBW(t *testing.T, serverURL string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bw")
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + calls + "\n" +
		"if [ \"$1\" = status ]; then printf '{\"status\":\"locked\",\"serverUrl\":\"" + serverURL + "\"}'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func ran(t *testing.T, calls, want string) bool {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), want)
}

// `bw config server` is refused once the CLI is logged in, and our state
// directory is persistent — so it must not be called when the server already
// matches. Skipping this check is what silently killed the sync for nine days.
func TestConfigureServerSkipsWhenAlreadyConfigured(t *testing.T) {
	const server = "https://vaultwarden.example.org"
	for _, tc := range []struct {
		name      string
		reported  string
		wantRecfg bool
	}{
		{"déjà configuré", server, false},
		{"slash final", server + "/", false},
		{"autre serveur", "https://other.example.org", true},
		{"jamais configuré", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, calls := fakeBW(t, tc.reported)
			c := &Client{BinPath: bin, Server: server, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := c.configureServer(context.Background()); err != nil {
				t.Fatalf("configureServer: %v", err)
			}
			if got := ran(t, calls, "config server"); got != tc.wantRecfg {
				t.Errorf("`bw config server` appelé = %v, attendu %v", got, tc.wantRecfg)
			}
		})
	}
}
