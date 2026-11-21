package cli

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseOptsGNU(t *testing.T) {
	type result struct {
		listen string
		force  bool
		rest   []string
	}
	parse := func(args ...string) (result, error) {
		var r result
		opts := []opt{
			stringOpt('l', "listen", "ADDR", "", &r.listen),
			boolOpt('f', "force", "", &r.force),
		}
		rest, err := parseOpts(opts, args)
		r.rest = rest
		return r, err
	}
	ok := []struct {
		args []string
		want result
	}{
		{[]string{"--listen", "a:1"}, result{listen: "a:1"}},
		{[]string{"--listen=a:1"}, result{listen: "a:1"}},
		{[]string{"--listen="}, result{}},
		{[]string{"-l", "a:1"}, result{listen: "a:1"}},
		{[]string{"-f"}, result{force: true}},
		{[]string{"--force"}, result{force: true}},
		{[]string{"x", "-f", "y"}, result{force: true, rest: []string{"x", "y"}}},
		{[]string{"-f", "--", "-l", "--force"}, result{force: true, rest: []string{"-l", "--force"}}},
		{[]string{"-"}, result{rest: []string{"-"}}},
	}
	for _, c := range ok {
		got, err := parse(c.args...)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parse(%q) = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
	for _, args := range [][]string{
		{"-listen", "a"}, // long name with one dash
		{"-force"},
		{"--l", "a"}, // short name with two dashes
		{"--f"},
		{"-lfoo"},
		{"-x"},
		{"--nope"},
		{"--listen"}, // missing value
		{"-l"},
		{"--force=yes"}, // bool with value
	} {
		if _, err := parse(args...); err == nil {
			t.Errorf("parse(%q): want error", args)
		}
	}
	for _, h := range []string{"-h", "--help"} {
		if _, err := parse(h); !errors.Is(err, errHelp) {
			t.Errorf("parse(%s) = %v, want errHelp", h, err)
		}
	}
}
