package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// The commands below run in bubbletea's goroutines, never in Update. Their
// texts match the CLI's output for the same action. A key value never
// reaches a message: the daemon's errors do not quote request bodies.

const restartHint = "restart Claude Code sessions to apply"

// putProviderCmd saves the provider and, when key is not empty, its key.
func putProviderCmd(c *admin.Client, name, baseURL, key string) tea.Cmd {
	return func() tea.Msg {
		cleared, err := c.PutProvider(name, baseURL)
		if err != nil {
			return doneMsg{err: err}
		}
		text := fmt.Sprintf("provider %s: %s", name, baseURL)
		if key != "" {
			state, err := c.SetKey(name, key)
			if err != nil {
				return doneMsg{text: text, err: err}
			}
			return doneMsg{text: text + fmt.Sprintf("\nkey for %s set: %s", name, state)}
		}
		if cleared {
			text += "\nits key was removed because the host changed; set it again with K"
		}
		return doneMsg{text: text}
	}
}

func deleteKeyCmd(c *admin.Client, name string) tea.Cmd {
	return func() tea.Msg {
		if err := c.DeleteKey(name); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: fmt.Sprintf("key for %s removed", name)}
	}
}

// testRouteCmd reports like shunt route test: a failed test is an error.
func testRouteCmd(c *admin.Client, name string) tea.Cmd {
	return func() tea.Msg {
		res, err := c.TestRoute(name)
		if err != nil {
			return doneMsg{err: err}
		}
		if text := admin.TestSummary(name, res); !res.OK {
			return doneMsg{err: errors.New(text)}
		} else {
			return doneMsg{text: text}
		}
	}
}

func setKeyCmd(c *admin.Client, name, value string) tea.Cmd {
	return func() tea.Msg {
		state, err := c.SetKey(name, value)
		if err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: fmt.Sprintf("key for %s set: %s", name, state)}
	}
}

func putRouteCmd(c *admin.Client, h Hooks, name, provider, model string) tea.Cmd {
	return func() tea.Msg {
		if _, err := c.PutRoute(name, provider, model); err != nil {
			return doneMsg{err: err}
		}
		text := fmt.Sprintf("route %s: model %s%s -> %s at %s", name, config.WirePrefix, name, model, provider)
		if admin.IsAlias(name) {
			text += fmt.Sprintf("\nwarning: while this route exists, a main thread using the %s alias gets 400 errors "+
				"(routes serve subagents only)", name)
		}
		return synced(h, name, text)
	}
}

func deleteProviderCmd(c *admin.Client, name string) tea.Cmd {
	return func() tea.Msg {
		if err := c.DeleteProvider(name); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: fmt.Sprintf("provider %s removed", name)}
	}
}

func deleteRouteCmd(c *admin.Client, h Hooks, name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := c.DeleteRoute(name); err != nil {
			return doneMsg{err: err}
		}
		return synced(h, name, fmt.Sprintf("route %s removed", name))
	}
}

// synced runs Hooks.Sync after a change to an alias-named route, as the
// CLI's route set|rm do; Sync itself does nothing while routing is off.
func synced(h Hooks, route, text string) doneMsg {
	if !admin.IsAlias(route) || h.Sync == nil {
		return doneMsg{text: text}
	}
	changes, err := h.Sync()
	if err != nil {
		return doneMsg{text: text, err: err}
	}
	if len(changes) > 0 {
		text += "\nupdated settings.json: " + strings.Join(changes, ", ") + "\n" + restartHint
	}
	return doneMsg{text: text}
}

func reloadCmd(c *admin.Client) tea.Cmd {
	return func() tea.Msg {
		if err := c.Reload(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "config reloaded"}
	}
}

// toggleCmd turns routing in settings.json off when on is set, else on.
func toggleCmd(h Hooks, on bool) tea.Cmd {
	if h.On == nil || h.Off == nil {
		return func() tea.Msg { return doneMsg{text: "routing cannot be changed from here"} }
	}
	if on {
		return func() tea.Msg {
			changed, err := h.Off()
			switch {
			case err != nil:
				return doneMsg{err: err}
			case !changed:
				return doneMsg{text: "routing already off"}
			}
			return doneMsg{text: "routing off in settings.json\n" + restartHint}
		}
	}
	return func() tea.Msg {
		changes, err := h.On()
		switch {
		case err != nil:
			return doneMsg{err: err}
		case len(changes) == 0:
			return doneMsg{text: "routing already on"}
		}
		return doneMsg{text: "routing on in settings.json: " + strings.Join(changes, ", ") + "\n" + restartHint}
	}
}
