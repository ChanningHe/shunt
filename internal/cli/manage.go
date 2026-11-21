package cli

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/term"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// maxKeyInput bounds what set-key reads from stdin.
const maxKeyInput = 8 << 10

// stdin is where set-key reads a key; tests replace it.
var stdin io.Reader = os.Stdin

// daemonOpts are the options of every command that talks to the daemon.
type daemonOpts struct{ dir, url string }

func newDaemonOpts() *daemonOpts { return &daemonOpts{dir: config.DefaultDir()} }

func (d *daemonOpts) opts(extra ...opt) []opt {
	return append([]opt{configDirOpt(&d.dir), urlOpt(&d.url)}, extra...)
}

func (d *daemonOpts) dial() (*admin.Client, error) { return admin.Dial(d.dir, d.url) }

func cmdProvider(stdout, stderr io.Writer, args []string) error {
	return runGroup(stdout, stderr, "provider", []command{
		{"list", "[options]", "list providers and the state of their keys", cmdProviderList},
		{"add", "NAME -b URL [--key-file PATH] [options]", "add a provider, or change its base URL", cmdProviderAdd},
		{"rm", "NAME [options]", "remove a provider and its key", cmdProviderRm},
		{"set-key", "NAME [--key-file PATH] [options]", "set a provider's key (read from stdin)", cmdProviderSetKey},
		{"rm-key", "NAME [options]", "remove a provider's key", cmdProviderRmKey},
	}, args)
}

func cmdRoute(stdout, stderr io.Writer, args []string) error {
	return runGroup(stdout, stderr, "route", []command{
		{"list", "[options]", "list routes", cmdRouteList},
		{"set", "NAME PROVIDER MODEL [options]", "send model shunt/NAME to MODEL at PROVIDER", cmdRouteSet},
		{"rm", "NAME [options]", "remove a route", cmdRouteRm},
		{"test", "NAME [options]", "send one small request through a route", cmdRouteTest},
	}, args)
}

func cmdProviderList(stdout, stderr io.Writer, args []string) error {
	d := newDaemonOpts()
	if _, err := parseArgs(stdout, "provider list", "[options]", d.opts(), args, 0); err != nil {
		return quietHelp(err)
	}
	_, st, err := fetchState(d.dir, d.url)
	if err != nil {
		return err
	}
	warnLoadError(stderr, st)
	printProviders(stdout, "", st)
	return nil
}

func keyFileOpt(dst *string) opt {
	return stringOpt(0, "key-file", "PATH", "store the key as a reference to this file (file://PATH), not its content", dst)
}

// keyFileRef turns a path into the keys-file reference file:///abs/path.
func keyFileRef(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return config.KeyFilePrefix + abs, nil
}

func cmdProviderAdd(stdout, stderr io.Writer, args []string) error {
	d, baseURL, keyFile := newDaemonOpts(), "", ""
	opts := d.opts(stringOpt('b', "base-url", "URL", "the provider's Anthropic-compatible base URL (https)", &baseURL), keyFileOpt(&keyFile))
	rest, err := parseArgs(stdout, "provider add", "NAME -b URL [--key-file PATH] [options]", opts, args, 1)
	if err != nil {
		return quietHelp(err)
	}
	if baseURL == "" {
		return errors.New("provider add needs -b/--base-url URL")
	}
	name, ref := rest[0], ""
	if keyFile != "" {
		if ref, err = keyFileRef(keyFile); err != nil {
			return err
		}
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	cleared, err := c.PutProvider(name, baseURL)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "provider %s: %s\n", name, baseURL)
	if cleared && ref == "" {
		fmt.Fprintf(stdout, "its key was removed because the host changed; set it again: shunt provider set-key %s\n", name)
	}
	if ref == "" {
		return nil
	}
	state, err := c.SetKey(name, ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "key: %s\n", state)
	return nil
}

func cmdProviderRm(stdout, stderr io.Writer, args []string) error {
	d := newDaemonOpts()
	rest, err := parseArgs(stdout, "provider rm", "NAME [options]", d.opts(), args, 1)
	if err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	if err := c.DeleteProvider(rest[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "provider %s removed\n", rest[0])
	return nil
}

// cmdProviderSetKey never takes a key as an argument: arguments end up in
// shell history and in every process listing.
func cmdProviderSetKey(stdout, stderr io.Writer, args []string) error {
	d, keyFile := newDaemonOpts(), ""
	rest, err := parseArgs(stdout, "provider set-key", "NAME [--key-file PATH] [options]", d.opts(keyFileOpt(&keyFile)), args, 1)
	if err != nil {
		return quietHelp(err)
	}
	name := rest[0]
	c, err := d.dial() // before asking for the key: fail early if the daemon is down
	if err != nil {
		return err
	}
	var value string
	if keyFile != "" {
		value, err = keyFileRef(keyFile)
	} else {
		value, err = readKey(stderr, name)
	}
	if err != nil {
		return err
	}
	state, err := c.SetKey(name, value)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "key for %s set: %s\n", name, state)
	return nil
}

func cmdProviderRmKey(stdout, stderr io.Writer, args []string) error {
	d := newDaemonOpts()
	rest, err := parseArgs(stdout, "provider rm-key", "NAME [options]", d.opts(), args, 1)
	if err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	if err := c.DeleteKey(rest[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "key for %s removed\n", rest[0])
	return nil
}

// readKey reads one line from stdin, without echo when it is a terminal.
func readKey(prompt io.Writer, name string) (string, error) {
	var line string
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(prompt, "key for %s (input hidden): ", name)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		line = string(b)
	} else {
		s, err := bufio.NewReader(io.LimitReader(stdin, maxKeyInput)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = s
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no key given on stdin")
	}
	return key, nil
}

func cmdRouteList(stdout, stderr io.Writer, args []string) error {
	d := newDaemonOpts()
	if _, err := parseArgs(stdout, "route list", "[options]", d.opts(), args, 0); err != nil {
		return quietHelp(err)
	}
	_, st, err := fetchState(d.dir, d.url)
	if err != nil {
		return err
	}
	warnLoadError(stderr, st)
	printRoutes(stdout, "", st)
	return nil
}

func cmdRouteSet(stdout, stderr io.Writer, args []string) error {
	d, settings := newDaemonOpts(), defaultSettingsPath()
	rest, err := parseArgs(stdout, "route set", "NAME PROVIDER MODEL [options]", d.opts(settingsOpt(&settings)), args, 3)
	if err != nil {
		return quietHelp(err)
	}
	name, provider, model := rest[0], rest[1], rest[2]
	c, err := d.dial()
	if err != nil {
		return err
	}
	if _, err := c.PutRoute(name, provider, model); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "route %s: model %s%s -> %s at %s\n", name, config.WirePrefix, name, model, provider)
	if !admin.IsAlias(name) {
		return nil
	}
	fmt.Fprintf(stderr, "shunt: warning: while this route exists, a main thread using the %s alias gets 400 errors "+
		"(routes serve subagents only)\n", name)
	return syncSettings(stdout, c, settings)
}

func cmdRouteRm(stdout, stderr io.Writer, args []string) error {
	d, settings := newDaemonOpts(), defaultSettingsPath()
	rest, err := parseArgs(stdout, "route rm", "NAME [options]", d.opts(settingsOpt(&settings)), args, 1)
	if err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	if _, err := c.DeleteRoute(rest[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "route %s removed\n", rest[0])
	if !admin.IsAlias(rest[0]) {
		return nil
	}
	return syncSettings(stdout, c, settings)
}

// cmdRouteTest fails (exit 1) when the provider does not answer 2xx.
func cmdRouteTest(stdout, stderr io.Writer, args []string) error {
	d := newDaemonOpts()
	rest, err := parseArgs(stdout, "route test", "NAME [options]", d.opts(), args, 1)
	if err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	res, err := c.TestRoute(rest[0])
	if err != nil {
		return err
	}
	text := admin.TestSummary(rest[0], res)
	if !res.OK {
		return errors.New(text)
	}
	fmt.Fprintln(stdout, text)
	return nil
}

// cmdActivity lists what the router did with its most recent requests.
func cmdActivity(stdout, stderr io.Writer, args []string) error {
	d, n := newDaemonOpts(), "20"
	opts := d.opts(stringOpt('n', "lines", "N", "show the last N requests (default 20)", &n))
	if _, err := parseArgs(stdout, "activity", "[options]", opts, args, 0); err != nil {
		return quietHelp(err)
	}
	limit, err := strconv.Atoi(n)
	if err != nil || limit < 0 {
		return fmt.Errorf("-n %q is not a count", n)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	a, err := c.Activity()
	if err != nil {
		return err
	}
	if len(a.Recent) == 0 {
		fmt.Fprintf(stdout, "no requests since the router started (%s)\n", a.Since.Local().Format("2006-01-02 15:04:05"))
		return nil
	}
	recent := a.Recent[:min(limit, len(a.Recent))]
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tTARGET\tSTATUS\tMS\tMODEL\tPATH")
	for _, e := range slices.Backward(recent) { // oldest first, like a log
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", e.Time.Local().Format("15:04:05"), e.Target, e.Status, e.Ms, cmp.Or(e.Model, "-"), e.Path)
	}
	return tw.Flush()
}

// syncSettings brings the alias variables of settings.json in line with
// the routes, but only while routing is on there.
func syncSettings(stdout io.Writer, c *admin.Client, path string) error {
	sf, changes, err := syncRoutes(c, path)
	if err != nil || len(changes) == 0 {
		return err
	}
	printChanges(stdout, "updated "+sf.path, changes)
	return nil
}

// syncRoutes is syncSettings without the output: it saves settings.json and
// returns what changed (nothing while routing is off).
func syncRoutes(c *admin.Client, path string) (*settingsFile, []string, error) {
	sf, err := loadSettings(path)
	if err != nil {
		return nil, nil, fmt.Errorf("route saved, but settings.json not synced: %w", err)
	}
	if !routingEnabled(sf) {
		return sf, nil, nil
	}
	st, err := c.State()
	if err != nil {
		return nil, nil, fmt.Errorf("route saved, but settings.json not synced: %w", err)
	}
	want := wantAliases(st.Config.Routes)
	if f := foreignKeys(sf, slices.Sorted(maps.Keys(want))); len(f) > 0 {
		return nil, nil, fmt.Errorf("route saved, but %s sets %v, which shunt did not write; replace it with: shunt on --force", sf.path, f)
	}
	changes, err := syncAliases(sf, want)
	if err != nil || len(changes) == 0 {
		return sf, changes, err
	}
	return sf, changes, sf.save()
}

func cmdReload(stdout, stderr io.Writer, args []string) error {
	d, pid, byPID := newDaemonOpts(), "", false
	pidOpt := opt{long: "pid", arg: "PID", help: "only send SIGHUP to this router process (no API, no token; for ExecReload)",
		set: func(v string) error { pid, byPID = v, true; return nil }}
	if _, err := parseArgs(stdout, "reload", "[options]", d.opts(pidOpt), args, 0); err != nil {
		return quietHelp(err)
	}
	if byPID {
		return signalReload(stdout, pid)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	if err := c.Reload(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "config reloaded")
	return nil
}

// signalReload sends SIGHUP to pid, which the router handles as a reload.
// Only positive pids: 0 and negatives address process groups.
func signalReload(stdout io.Writer, pid string) error {
	n, err := parsePID(pid)
	if err != nil {
		return err
	}
	if err := syscall.Kill(n, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal process %d: %w", n, err)
	}
	fmt.Fprintf(stdout, "sent SIGHUP to %d\n", n)
	return nil
}

// parsePID accepts only a single process: kill(2) treats 0 as the caller's
// process group and -1 as every process of the user.
func parsePID(pid string) (int, error) {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("--pid %q is not a process id", pid)
	}
	return n, nil
}

func cmdWeb(stdout, stderr io.Writer, args []string) error {
	d, noOpen := newDaemonOpts(), false
	opts := d.opts(boolOpt(0, "no-open", "print the link only; do not start a browser", &noOpen))
	if _, err := parseArgs(stdout, "web", "[options]", opts, args, 0); err != nil {
		return quietHelp(err)
	}
	c, err := d.dial()
	if err != nil {
		return err
	}
	wc, err := c.WebCode()
	if err != nil {
		return err
	}
	link := c.URL() + admin.Prefix + "/#code=" + wc.Code
	fmt.Fprintf(stdout, "%s\n(one-time link, valid for %d seconds)\n", link, wc.ExpiresIn)
	if noOpen {
		return nil
	}
	if err := openBrowser(link); err != nil {
		fmt.Fprintf(stderr, "shunt: could not start a browser (%v); open the link above\n", err)
	}
	return nil
}

func openBrowser(link string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, link)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func printProviders(w io.Writer, indent string, st *admin.State) {
	if len(st.Config.Providers) == 0 {
		fmt.Fprintf(w, "%sno providers; add one with: shunt provider add NAME -b URL\n", indent)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%sNAME\tKEY\tBASE_URL\n", indent)
	for _, name := range slices.Sorted(maps.Keys(st.Config.Providers)) {
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", indent, name, st.Keys[name], st.Config.Providers[name].BaseURL)
	}
	tw.Flush()
}

func printRoutes(w io.Writer, indent string, st *admin.State) {
	if len(st.Config.Routes) == 0 {
		fmt.Fprintf(w, "%sno routes; add one with: shunt route set NAME PROVIDER MODEL\n", indent)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%sNAME\tPROVIDER\tMODEL\tMODEL_ID\n", indent)
	for _, name := range slices.Sorted(maps.Keys(st.Config.Routes)) {
		r := st.Config.Routes[name]
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\n", indent, name, r.Provider, r.Model, config.WirePrefix+name)
	}
	tw.Flush()
}

// printDaemonState is the router part of shunt status.
func printDaemonState(w io.Writer, st *admin.State) {
	if st.LoadError != nil {
		fmt.Fprintf(w, "config error (fix the files, then: shunt reload): %s\n", *st.LoadError)
	}
	fmt.Fprintln(w, "routes:")
	printRoutes(w, "  ", st)
	fmt.Fprintln(w, "providers:")
	printProviders(w, "  ", st)
}
