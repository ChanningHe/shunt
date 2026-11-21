package cli

import (
	"errors"
	"io"
	"os"

	"golang.org/x/term"

	"shunt/internal/admin"
	"shunt/internal/tui"
)

func cmdTUI(stdout, stderr io.Writer, args []string) error {
	d, settings := newDaemonOpts(), defaultSettingsPath()
	if _, err := parseArgs(stdout, "tui", "[options]", d.opts(settingsOpt(&settings)), args, 0); err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	if _, err := c.State(); err != nil { // fail like the other commands, before taking the screen
		return err
	}
	if f, ok := stdout.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		return errors.New("shunt tui needs a terminal; in scripts use shunt provider and shunt route")
	}
	return tui.Run(c, tuiHooks(c, d, settings))
}

// tuiHooks gives the TUI the settings.json actions of on, off, status and
// route set|rm.
func tuiHooks(c *admin.Client, d *daemonOpts, settings string) tui.Hooks {
	return tui.Hooks{
		RoutingStatus: func() (bool, []string, error) {
			sf, err := loadSettings(settings)
			if err != nil || !routingEnabled(sf) {
				return false, nil, err
			}
			st, err := c.State()
			if err != nil {
				return true, nil, err
			}
			return true, aliasDrift(sf, wantAliases(st.Config.Routes)), nil
		},
		On: func() ([]string, error) {
			// Warnings go nowhere: the TUI shows the load error itself.
			_, changes, err := routingOn(io.Discard, d.dir, d.url, settings, false)
			return changes, err
		},
		Off: func() (bool, error) {
			_, changed, err := routingOff(settings, false)
			return changed, err
		},
		Sync: func() ([]string, error) {
			_, changes, err := syncRoutes(c, settings)
			return changes, err
		},
	}
}
