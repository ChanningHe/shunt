package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shunt/internal/admin"
	"shunt/internal/config"
)

func TestServeOptions(t *testing.T) {
	dir := t.TempDir()
	if _, err := runCmd(t, "serve", "--config-dir", dir, "--config", "x.json"); err == nil || !strings.Contains(err.Error(), "unknown option --config") {
		t.Errorf("--config: err = %v", err)
	}
	if _, err := runCmd(t, "serve", "--config-dir", dir, "-l", "0.0.0.0:27486"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("non-loopback -l: err = %v", err)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := runCmd(t, "serve", "--config-dir", dir, "-l", busy.Addr().String()); err == nil {
		t.Error("bind failure on -l address did not fail serve")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("-l was written to the config dir: %v", entries)
	}
	// Without -l the address comes from the config file.
	writeTestFile(t, filepath.Join(dir, "config.toml"), "listen = '"+busy.Addr().String()+"'\n")
	if _, err := runCmd(t, "serve", "--config-dir", dir); err == nil {
		t.Error("bind failure on the configured address did not fail serve")
	}
	if got := config.Open(dir).Load().Config.Listen; got != busy.Addr().String() {
		t.Errorf("config listen changed to %q", got)
	}
}

// A second serve on a busy port fails before it writes a token, so the
// running daemon's token stays valid.
func TestServeBusyPortKeepsToken(t *testing.T) {
	dir := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, _ := net.SplitHostPort(busy.Addr().String())
	path, err := admin.WriteToken(dir, port, "running-daemon-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "serve", "--config-dir", dir, "-l", busy.Addr().String()); err == nil {
		t.Fatal("serve on a busy port succeeded")
	}
	if got, _ := os.ReadFile(path); string(got) != "running-daemon-token\n" {
		t.Errorf("token file changed to %q", got)
	}
}
