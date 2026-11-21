package cli

import (
	"fmt"
	"io"
	"strings"
)

// opt is one GNU-style option: "-x" short form and/or "--long" form.
// Options with a non-empty arg take a value ("--long=V", "--long V", "-x V").
type opt struct {
	short byte   // 0 if there is no short form
	long  string // without leading dashes
	arg   string // value placeholder for help; "" means boolean
	help  string
	set   func(string) error
}

func stringOpt(short byte, long, arg, help string, dst *string) opt {
	return opt{short: short, long: long, arg: arg, help: help, set: func(v string) error { *dst = v; return nil }}
}

func boolOpt(short byte, long, help string, dst *bool) opt {
	return opt{short: short, long: long, help: help, set: func(string) error { *dst = true; return nil }}
}

// errHelp is returned when -h/--help was given; the caller prints usage.
var errHelp = fmt.Errorf("help requested")

// parseOpts parses GNU-style options and returns the remaining positional
// arguments. "--" ends option parsing. A single dash must be followed by
// exactly one letter; long names need two dashes.
func parseOpts(opts []opt, args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(rest, args[i+1:]...), nil
		case a == "-h" || a == "--help":
			return nil, errHelp
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			o, ok := findLong(opts, name)
			if !ok || len(name) < 2 {
				return nil, fmt.Errorf("unknown option %s", a)
			}
			n, err := apply(o, a, val, hasVal, args[i+1:])
			if err != nil {
				return nil, err
			}
			i += n
		case strings.HasPrefix(a, "-") && a != "-":
			if len(a) != 2 {
				return nil, fmt.Errorf("unknown option %s (long options take two dashes)", a)
			}
			o, ok := findShort(opts, a[1])
			if !ok {
				return nil, fmt.Errorf("unknown option %s", a)
			}
			n, err := apply(o, a, "", false, args[i+1:])
			if err != nil {
				return nil, err
			}
			i += n
		default:
			rest = append(rest, a)
		}
	}
	return rest, nil
}

// apply sets o and reports how many following args it consumed.
func apply(o opt, flag, val string, hasVal bool, next []string) (int, error) {
	if o.arg == "" {
		if hasVal {
			return 0, fmt.Errorf("option %s takes no value", flag)
		}
		return 0, o.set("")
	}
	if hasVal {
		return 0, o.set(val)
	}
	if len(next) == 0 {
		return 0, fmt.Errorf("option %s needs a value", flag)
	}
	return 1, o.set(next[0])
}

func findLong(opts []opt, name string) (opt, bool) {
	for _, o := range opts {
		if o.long == name {
			return o, true
		}
	}
	return opt{}, false
}

func findShort(opts []opt, c byte) (opt, bool) {
	for _, o := range opts {
		if o.short != 0 && o.short == c {
			return o, true
		}
	}
	return opt{}, false
}

func printOpts(w io.Writer, opts []opt) {
	for _, o := range append(opts, opt{short: 'h', long: "help", help: "show this help"}) {
		short := "    "
		if o.short != 0 {
			short = fmt.Sprintf("-%c, ", o.short)
		}
		long := "--" + o.long
		if o.arg != "" {
			long += " " + o.arg
		}
		fmt.Fprintf(w, "  %s%-22s %s\n", short, long, o.help)
	}
}
