package cli

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// s7-2: the CLI cannot reach provider a's key through the name "a/key".
func TestProviderRmRejectsSlashName(t *testing.T) {
	d := startDaemon(t, "[providers.a]\nbase_url = \"https://a.example\"\n", "a = \"sk-a\"\n")
	keys := readDisk(t, filepath.Join(d.dir, "keys.toml"))
	if _, err := d.run(t, "provider", "rm", "a/key"); err == nil {
		t.Error("provider rm a/key succeeded")
	}
	if got := readDisk(t, filepath.Join(d.dir, "keys.toml")); got != keys {
		t.Errorf("keys file changed:\n%s", got)
	}
}

// s7-4: reload --pid only signals; it needs neither the daemon nor a token.
func TestReloadByPID(t *testing.T) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	empty := t.TempDir()
	if out, err := runCmd(t, "reload", "--pid", strconv.Itoa(os.Getpid()), "--config-dir", empty); err != nil {
		t.Fatalf("reload --pid: %v %s", err, out)
	}
	select {
	case <-hup:
	case <-time.After(3 * time.Second):
		t.Fatal("SIGHUP not delivered")
	}

	// Invalid pids are checked on parsePID only, never through the command:
	// kill(0) and kill(-1) would signal the process group or every process
	// of the user.
	for _, pid := range []string{"0", "-1", "-12345", "abc", "1.5", ""} {
		if n, err := parsePID(pid); err == nil {
			t.Errorf("parsePID(%q) = %d, want an error", pid, n)
		}
	}
}

// s6-2: after a failed reload, status shows the routes still being served
// next to the error, not an empty table.
func TestStatusAfterFailedReloadShowsRunningRoutes(t *testing.T) {
	d := startDaemon(t, routesCfg, "")
	link, _ := writeLinkedSettings(t, "{}\n")
	writeTestFile(t, filepath.Join(d.dir, "config.toml"), "[routes\n")
	if _, err := d.run(t, "reload"); err == nil {
		t.Fatal("reload of a broken file succeeded")
	}
	out := mustRun(t, d, "status", "-s", link)
	if !strings.Contains(out, "config error") || !strings.Contains(out, "shunt/sonnet") || strings.Contains(out, "no routes") {
		t.Errorf("status after a failed reload:\n%s", out)
	}
}
