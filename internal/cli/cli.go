// Package cli is the shunt command line: the router process (serve), the
// clients of its admin API (provider, route, reload, activity, tui, web) and
// the commands that edit Claude Code's settings.json (on, off, status, claude).
package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const dialTimeout = time.Second

type command struct {
	name, args, summary string
	run                 func(stdout, stderr io.Writer, args []string) error
}

func commands() []command {
	return []command{
		{"serve", "[options]", "run the router (used by the systemd/launchd units)", cmdServe},
		{"provider", "<command>", "list, add, remove providers and set their keys", cmdProvider},
		{"route", "<command>", "list, set, remove, test routes (model shunt/NAME)", cmdRoute},
		{"reload", "[options]", "make the router re-read its config files", cmdReload},
		{"activity", "[options]", "show the router's most recent requests", cmdActivity},
		{"tui", "[options]", "manage providers and routes in a terminal UI", cmdTUI},
		{"web", "[options]", "open the web UI with a one-time login link", cmdWeb},
		{"on", "[options]", "enable routing in Claude Code's settings.json env", cmdOn},
		{"off", "[options]", "remove the routing keys from settings.json env", cmdOff},
		{"status", "[options]", "show settings.json state, the router and its routes", cmdStatus},
		{"claude", "[options --] [claude args...]", "start claude with routing set via environment variables", cmdClaude},
	}
}

// Run executes the shunt command line args, writing to stdout and stderr.
func Run(stdout, stderr io.Writer, args []string) error {
	return runGroup(stdout, stderr, "", commands(), args)
}

// runGroup runs the command named by args[0] from cmds; group is the
// command path so far ("" at the top level).
func runGroup(stdout, stderr io.Writer, group string, cmds []command, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stdout, group, cmds)
		return nil
	}
	for _, c := range cmds {
		if c.name == args[0] {
			return c.run(stdout, stderr, args[1:])
		}
	}
	usage(stderr, group, cmds)
	return fmt.Errorf("unknown command %q", strings.TrimSpace(group+" "+args[0]))
}

func usage(w io.Writer, group string, cmds []command) {
	name := strings.TrimSpace("shunt " + group)
	fmt.Fprintf(w, "usage: %s <command> [options]\n\ncommands:\n", name)
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nrun '%s <command> --help' for its options\n", name)
}

// parseCmd parses options for a subcommand, printing help on -h/--help.
// It returns errHelp after printing so the caller can stop quietly.
func parseCmd(w io.Writer, name, args string, opts []opt, argv []string) ([]string, error) {
	rest, err := parseOpts(opts, argv)
	if errors.Is(err, errHelp) {
		fmt.Fprintf(w, "usage: shunt %s %s\n\noptions:\n", name, args)
		printOpts(w, opts)
	}
	return rest, err
}

// parseArgs is parseCmd plus an exact number of positional arguments.
func parseArgs(w io.Writer, name, args string, opts []opt, argv []string, n int) ([]string, error) {
	rest, err := parseCmd(w, name, args, opts, argv)
	if err != nil {
		return nil, err
	}
	if len(rest) != n {
		return nil, fmt.Errorf("usage: shunt %s %s", name, args)
	}
	return rest, nil
}

func noPositional(rest []string) error {
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
	}
	return nil
}

func defaultSettingsPath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func settingsOpt(dst *string) opt {
	return stringOpt('s', "settings", "FILE", "Claude Code settings file (default ~/.claude/settings.json)", dst)
}

func configDirOpt(dst *string) opt {
	return stringOpt(0, "config-dir", "DIR", "config directory (default "+*dst+")", dst)
}

func urlOpt(dst *string) opt {
	return stringOpt('u', "url", "URL", "router URL (default: http:// + the config's listen address)", dst)
}

// fetchState reads the routes and key states from the daemon.
func fetchState(dir, routerURL string) (*admin.Client, *admin.State, error) {
	c, err := admin.Dial(dir, routerURL)
	if err != nil {
		return nil, nil, err
	}
	st, err := c.State()
	if err != nil {
		return nil, nil, err
	}
	return c, st, nil
}

func cmdOn(stdout, stderr io.Writer, args []string) error {
	settings, dir, routerURL, force := defaultSettingsPath(), config.DefaultDir(), "", false
	opts := []opt{settingsOpt(&settings), configDirOpt(&dir), urlOpt(&routerURL),
		boolOpt('f', "force", "write even if the router is unreachable or a value shunt did not write is set", &force)}
	rest, err := parseCmd(stdout, "on", "[options]", opts, args)
	if err != nil {
		return quietHelp(err)
	}
	if err := noPositional(rest); err != nil {
		return err
	}
	sf, changes, err := routingOn(stderr, dir, routerURL, settings, force)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "routing already on in %s\n", sf.path)
		return nil
	}
	printChanges(stdout, "routing on in "+sf.path, changes)
	return nil
}

// routingOn points settings.json at the router, with the alias variables
// of its routes, and saves it. It returns the file and what changed.
func routingOn(stderr io.Writer, dir, routerURL, settings string, force bool) (*settingsFile, []string, error) {
	base, err := admin.DaemonURL(dir, routerURL)
	if err != nil {
		return nil, nil, err
	}
	var want map[string]string // nil: routes unknown, alias variables untouched
	switch _, st, err := fetchState(dir, routerURL); {
	case err == nil:
		want = wantAliases(st.Config.Routes)
		warnLoadError(stderr, st)
	case !force:
		return nil, nil, fmt.Errorf("%w\nwith routing on and the router down, every Claude Code request fails; --force writes anyway", err)
	default:
		fmt.Fprintf(stderr, "shunt: warning: %v\nalias variables left as they are; run shunt on again once the router is up\n", err)
	}
	sf, err := loadSettings(settings)
	if err != nil {
		return nil, nil, err
	}
	changes, err := enableRouting(sf, base, want, force)
	if err != nil || len(changes) == 0 {
		return sf, changes, err
	}
	return sf, changes, sf.save()
}

func printChanges(w io.Writer, title string, changes []string) {
	fmt.Fprintf(w, "%s:\n", title)
	for _, c := range changes {
		fmt.Fprintf(w, "  %s\n", c)
	}
	fmt.Fprintln(w, "restart Claude Code sessions to apply")
}

func warnLoadError(w io.Writer, st *admin.State) {
	if st.LoadError != nil {
		fmt.Fprintf(w, "shunt: warning: the router's config failed to load (routes may be stale): %s\n", *st.LoadError)
	}
}

func cmdOff(stdout, stderr io.Writer, args []string) error {
	settings, force := defaultSettingsPath(), false
	opts := []opt{settingsOpt(&settings), boolOpt('f', "force", "also remove values shunt did not write", &force)}
	rest, err := parseCmd(stdout, "off", "[options]", opts, args)
	if err != nil {
		return quietHelp(err)
	}
	if err := noPositional(rest); err != nil {
		return err
	}
	sf, changed, err := routingOff(settings, force)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(stdout, "routing already off in %s\n", sf.path)
		return nil
	}
	fmt.Fprintf(stdout, "routing off in %s\nrestart Claude Code sessions to apply\n", sf.path)
	return nil
}

// routingOff removes the routing values from settings.json and saves it if
// anything changed.
func routingOff(settings string, force bool) (*settingsFile, bool, error) {
	sf, err := loadSettings(settings)
	if err != nil {
		return nil, false, err
	}
	changed, err := disableRouting(sf, force)
	if err != nil || !changed {
		return sf, changed, err
	}
	return sf, true, sf.save()
}

// routingEnabled reports whether settings.json points Claude Code at a
// shunt router.
func routingEnabled(sf *settingsFile) bool {
	base, ok := sf.envString(envBaseURL)
	return ok && ours(envBaseURL, base)
}

func cmdStatus(stdout, stderr io.Writer, args []string) error {
	settings, dir, routerURL := defaultSettingsPath(), config.DefaultDir(), ""
	opts := []opt{settingsOpt(&settings), configDirOpt(&dir), urlOpt(&routerURL)}
	rest, err := parseCmd(stdout, "status", "[options]", opts, args)
	if err != nil {
		return quietHelp(err)
	}
	if err := noPositional(rest); err != nil {
		return err
	}
	sf, err := loadSettings(settings)
	if err != nil {
		return err
	}
	base, _ := sf.envString(envBaseURL)
	on := routingEnabled(sf)
	state := "off"
	if on {
		state = "on"
	}
	fmt.Fprintf(stdout, "settings: %s (%s)\n", state, sf.path)
	for _, key := range routingVars() {
		val, _ := sf.envString(key)
		if val == "" {
			continue
		}
		note := ""
		if !ours(key, val) {
			note = " (not written by shunt)"
		}
		fmt.Fprintf(stdout, "  %s=%s%s\n", key, val, note)
	}
	if routerURL == "" {
		routerURL = "http://" + config.DefaultListen
		if on {
			routerURL = base
		} else if u, err := admin.DaemonURL(dir, ""); err == nil {
			routerURL = u
		}
	}
	if err := reachable(routerURL); err != nil {
		fmt.Fprintf(stdout, "router: unreachable (%v)\n", err)
		if on {
			return errors.New("routing is on but the router is down: Claude Code requests will fail (shunt off to disable)")
		}
		return nil
	}
	fmt.Fprintf(stdout, "router: reachable at %s\n", routerURL)
	_, st, err := fetchState(dir, routerURL)
	if err != nil {
		fmt.Fprintf(stdout, "admin API: %v\n", err)
		return nil
	}
	printDaemonState(stdout, st)
	if drift := aliasDrift(sf, wantAliases(st.Config.Routes)); on && len(drift) > 0 {
		fmt.Fprintf(stdout, "settings.json does not match the alias routes (%s); run: shunt on\n", strings.Join(drift, "; "))
	}
	return nil
}

// bgArgs are claude invocations served by the background daemon. The spike
// showed such sessions do not reliably inherit the caller's environment, so
// env mode would silently bypass the router.
func usesDaemon(claudeArgs []string) bool {
	if len(claudeArgs) > 0 && claudeArgs[0] == "agents" {
		return true
	}
	return slices.Contains(claudeArgs, "--bg") || slices.Contains(claudeArgs, "--background")
}

// splitClaudeArgs: with a literal "--", options before it are shunt's and
// everything after goes to claude; without it, everything goes to claude.
func splitClaudeArgs(args []string) (own, claude []string) {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i], args[i+1:]
	}
	return nil, args
}

func cmdClaude(stdout, stderr io.Writer, args []string) error {
	own, claudeArgs := splitClaudeArgs(args)
	dir, routerURL := config.DefaultDir(), ""
	// Long names only, distinct from claude's own options (claude has --model).
	opts := []opt{
		stringOpt(0, "router-url", "URL", "router URL (default: http:// + the config's listen address)", &routerURL),
		configDirOpt(&dir),
	}
	rest, err := parseCmd(stdout, "claude", "[options --] [claude args...]", opts, own)
	if err != nil {
		return quietHelp(err)
	}
	if err := noPositional(rest); err != nil {
		return err
	}
	if usesDaemon(claudeArgs) {
		return errors.New("background sessions (--bg, claude agents) run under a shared daemon and may not see " +
			"this environment, silently bypassing the router; use 'shunt on' (settings.json mode) for them")
	}
	c, st, err := fetchState(dir, routerURL)
	if err != nil {
		return err
	}
	warnLoadError(stderr, st)
	if sf, err := loadSettings(defaultSettingsPath()); err == nil {
		if cur, ok := sf.envString(envBaseURL); ok && cur != c.URL() {
			fmt.Fprintf(stderr, "shunt: warning: settings.json sets %s=%s, which overrides the environment\n", envBaseURL, cur)
		}
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	env := routingEnv(os.Environ(), c.URL(), wantAliases(st.Config.Routes))
	return syscall.Exec(bin, append([]string{"claude"}, claudeArgs...), env)
}

// routingEnv returns environ with the base URL and the alias variables of
// want set; other alias variables holding a shunt/ id are dropped, ones the
// user set are kept.
func routingEnv(environ []string, routerURL string, want map[string]string) []string {
	out := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		key, val, _ := strings.Cut(kv, "=")
		_, wanted := want[key]
		return key == envBaseURL || wanted || ours(key, val)
	})
	out = append(out, envBaseURL+"="+routerURL)
	for _, key := range slices.Sorted(maps.Keys(want)) {
		out = append(out, key+"="+want[key])
	}
	return out
}

func reachable(routerURL string) error {
	u, err := url.Parse(routerURL)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := net.DialTimeout("tcp", host, dialTimeout)
	if err != nil {
		return fmt.Errorf("router not reachable at %s", routerURL)
	}
	return conn.Close()
}

func quietHelp(err error) error {
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}
