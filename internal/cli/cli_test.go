package cli

import (
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSplitClaudeArgs(t *testing.T) {
	cases := []struct {
		args, own, claude []string
	}{
		{[]string{"-c", "--model", "opus"}, nil, []string{"-c", "--model", "opus"}},
		{[]string{"--router-url", "http://127.0.0.1:9", "--", "-p", "hi"}, []string{"--router-url", "http://127.0.0.1:9"}, []string{"-p", "hi"}},
		{[]string{"--"}, []string{}, []string{}},
		{nil, nil, nil},
	}
	for _, c := range cases {
		own, claude := splitClaudeArgs(c.args)
		if !reflect.DeepEqual(own, c.own) || !reflect.DeepEqual(claude, c.claude) {
			t.Errorf("split(%q) = %q, %q; want %q, %q", c.args, own, claude, c.own, c.claude)
		}
	}
}

func TestClaudeRefusesDaemonSessions(t *testing.T) {
	for _, args := range [][]string{{"--bg", "do x"}, {"-p", "x", "--background"}, {"agents"}} {
		out, err := runCmd(t, append([]string{"claude"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "shunt on") {
			t.Errorf("claude %q: err=%v out=%s; want refusal pointing at 'shunt on'", args, err, out)
		}
	}
	if usesDaemon([]string{"-p", "explain agents"}) {
		t.Error("prompt text mentioning agents must not count as the agents subcommand")
	}
}

func TestClaudeRefusesWhenRouterDown(t *testing.T) {
	_, err := runCmd(t, "claude", "--router-url", "http://127.0.0.1:1", "--", "-p", "hi")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("err = %v, want router not reachable", err)
	}
}

func TestRoutingEnv(t *testing.T) {
	in := []string{"PATH=/bin", envBaseURL + "=https://old", aliasVar("sonnet") + "=x", envBaseURL + "_EXTRA=keep",
		aliasVar("opus") + "=shunt/opus", aliasVar("fable") + "=claude-fable-1"}
	want := map[string]string{aliasVar("sonnet"): "shunt/sonnet", aliasVar("haiku"): "shunt/haiku"}
	got := routingEnv(in, "http://127.0.0.1:27486", want)
	exp := []string{"PATH=/bin", envBaseURL + "_EXTRA=keep", aliasVar("fable") + "=claude-fable-1", envBaseURL + "=http://127.0.0.1:27486",
		aliasVar("haiku") + "=shunt/haiku", aliasVar("sonnet") + "=shunt/sonnet"}
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("routingEnv = %q, want %q", got, exp)
	}
	if !slices.Equal(in[:2], []string{"PATH=/bin", envBaseURL + "=https://old"}) {
		t.Error("input environ was mutated")
	}
}

func TestReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := reachable("http://" + ln.Addr().String()); err != nil {
		t.Errorf("listening router reported unreachable: %v", err)
	}
	if err := reachable("http://127.0.0.1:1"); err == nil {
		t.Error("closed port reported reachable")
	}
}

func TestDispatchUnknownAndHelp(t *testing.T) {
	if _, err := runCmd(t, "nope"); err == nil {
		t.Error("unknown command accepted")
	}
	for _, args := range [][]string{{}, {"--help"}, {"serve", "--help"}, {"on", "-h"}, {"claude", "--help", "--"}} {
		if out, err := runCmd(t, args...); err != nil || !strings.Contains(out, "usage: shunt") {
			t.Errorf("%q: err=%v out=%q", args, err, out)
		}
	}
	if _, err := runCmd(t, "serve", "-listen", "127.0.0.1:0"); err == nil {
		t.Error("single-dash long option accepted")
	}
}

// Review 2: shunt's own options must not shadow claude's (--model).
// --sonnet-model is gone: aliases follow the routes.
func TestClaudeOwnOptionsDoNotShadowClaude(t *testing.T) {
	for _, flag := range []string{"--model", "--sonnet-model"} {
		_, err := runCmd(t, "claude", flag, "opus", "--", "-p", "hi")
		if err == nil || !strings.Contains(err.Error(), "unknown option "+flag) {
			t.Errorf("err = %v; want unknown option %s", err, flag)
		}
	}
	for _, args := range [][]string{{"on", "-m", "x"}, {"on", "--model", "x"}} {
		if _, err := runCmd(t, args...); err == nil || !strings.Contains(err.Error(), "unknown option") {
			t.Errorf("%q: err = %v", args, err)
		}
	}
}
