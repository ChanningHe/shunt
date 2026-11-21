package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shunt/internal/admin"
)

func TestTUINeedsTerminal(t *testing.T) {
	d := startDaemon(t, "", "")
	if _, err := d.run(t, "tui"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v", err)
	}
	out, err := runCmd(t, "--help")
	if err != nil || !strings.Contains(out, "tui") {
		t.Errorf("usage lacks tui: %v\n%s", err, out)
	}
}

// Without a daemon, tui fails like every other config command.
func TestTUIWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--config-dir", dir, "-u", "http://127.0.0.1:1"}
	_, want := runCmd(t, append([]string{"route", "list"}, args...)...)
	_, err := runCmd(t, append([]string{"tui"}, args...)...)
	if err == nil || want == nil || err.Error() != want.Error() {
		t.Fatalf("tui: %v\nroute list: %v", err, want)
	}
}

// The hooks act on settings.json exactly as on, off, status and route set do.
func TestTUIHooks(t *testing.T) {
	d := startDaemon(t, routesCfg, "")
	link, real := writeLinkedSettings(t, sampleSettings)
	c, err := admin.Dial(d.dir, d.url)
	if err != nil {
		t.Fatal(err)
	}
	h := tuiHooks(c, &daemonOpts{dir: d.dir, url: d.url}, link)

	if on, drift, err := h.RoutingStatus(); on || drift != nil || err != nil {
		t.Fatalf("status before on: %v %v %v", on, drift, err)
	}
	changes, err := h.On()
	if err != nil || !slices.Contains(changes, envBaseURL+"="+d.url) || !slices.Contains(changes, aliasVar("haiku")+"=shunt/haiku") {
		t.Fatalf("On = %q, %v", changes, err)
	}
	if on, drift, err := h.RoutingStatus(); !on || len(drift) != 0 || err != nil {
		t.Fatalf("status after on: %v %v %v", on, drift, err)
	}

	if _, err := c.PutRoute("opus", "deepseek", "m"); err != nil {
		t.Fatal(err)
	}
	if _, drift, _ := h.RoutingStatus(); len(drift) != 1 || !strings.Contains(drift[0], aliasVar("opus")) {
		t.Fatalf("drift = %q", drift)
	}
	if changes, err := h.Sync(); err != nil || !slices.Equal(changes, []string{aliasVar("opus") + "=shunt/opus"}) {
		t.Fatalf("Sync = %q, %v", changes, err)
	}

	if changed, err := h.Off(); !changed || err != nil {
		t.Fatalf("Off = %v, %v", changed, err)
	}
	if got := readDisk(t, real); got != sampleSettings {
		t.Errorf("after on+off settings differ:\n%s", got)
	}
	if changes, err := h.Sync(); err != nil || len(changes) != 0 {
		t.Errorf("Sync while off = %q, %v", changes, err)
	}
	if _, _, err := tuiHooks(c, &daemonOpts{dir: d.dir, url: d.url}, filepath.Join(t.TempDir(), "x", "s.json")).RoutingStatus(); err != nil {
		t.Errorf("missing settings file: %v", err)
	}
}
