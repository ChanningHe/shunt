package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// maxInput bounds a typed value; keys and URLs are far shorter.
const maxInput = 4096

// field is a text input, or a choice among fixed values when choices is set.
type field struct {
	label    string
	input    textinput.Model
	choices  []string
	choice   int
	optional bool // may be left empty
}

func (f *field) value() string {
	if f.choices != nil {
		return f.choices[f.choice]
	}
	return strings.TrimSpace(f.input.Value())
}

// form collects values and turns them into one API command.
type form struct {
	title  string
	hint   string
	fields []field
	focus  int
	err    error
	send   func(values []string) tea.Cmd
}

func textField(label, value string) field {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = maxInput
	st := in.Styles()
	st.Cursor.Blink = false // a blinking cursor would tick forever
	in.SetStyles(st)
	in.SetValue(value)
	return field{label: label, input: in}
}

// keyField hides what is typed.
func keyField() field {
	key := textField("key", "")
	key.input.EchoMode = textinput.EchoNone
	return key
}

const keyHint = "key input is hidden; " + config.KeyFilePrefix + "/abs/path stores a reference to that file"

func addProviderForm(c *admin.Client) *form {
	key := keyField()
	key.optional = true
	return &form{
		title:  "add provider",
		hint:   "the key is optional here (K sets it later); " + keyHint,
		fields: []field{textField("name", ""), textField("base URL", ""), key},
		send:   func(v []string) tea.Cmd { return putProviderCmd(c, v[0], v[1], v[2]) },
	}
}

func editProviderForm(c *admin.Client, name, baseURL string) *form {
	return &form{
		title:  "provider " + name,
		hint:   "changing the host removes a literal key",
		fields: []field{textField("base URL", baseURL)},
		send:   func(v []string) tea.Cmd { return putProviderCmd(c, name, v[0], "") },
	}
}

func setKeyForm(c *admin.Client, name string) *form {
	return &form{
		title:  "key for " + name,
		hint:   keyHint,
		fields: []field{keyField()},
		send:   func(v []string) tea.Cmd { return setKeyCmd(c, name, v[0]) },
	}
}

// routeForm adds a route (name "") or edits the named route r. The provider
// is picked from the existing ones.
func routeForm(c *admin.Client, h Hooks, name string, r config.Route, providers []string) *form {
	prov := field{label: "provider", choices: providers, choice: max(0, slices.Index(providers, r.Provider))}
	f := &form{title: "route " + name, fields: []field{prov, textField("model", r.Model)}}
	f.send = func(v []string) tea.Cmd { return putRouteCmd(c, h, name, v[0], v[1]) }
	if name == "" {
		f.title = "add route"
		f.fields = append([]field{textField("name", "")}, f.fields...)
		f.send = func(v []string) tea.Cmd { return putRouteCmd(c, h, v[0], v[1], v[2]) }
	}
	f.hint = "model id is " + config.WirePrefix + "<name>; ←/→ picks the provider"
	return f
}

// submit validates the form and returns the command that sends it.
func (f *form) submit() (tea.Cmd, error) {
	values := make([]string, len(f.fields))
	for i := range f.fields {
		if values[i] = f.fields[i].value(); values[i] == "" && !f.fields[i].optional {
			return nil, fmt.Errorf("%s is required", f.fields[i].label)
		}
	}
	return f.send(values), nil
}

func (f *form) focusField(i int) tea.Cmd {
	f.focus = (i + len(f.fields)) % len(f.fields)
	var cmd tea.Cmd
	for j := range f.fields {
		if f.fields[j].choices != nil { // no text input behind it
			continue
		}
		if j == f.focus {
			cmd = f.fields[j].input.Focus()
		} else {
			f.fields[j].input.Blur()
		}
	}
	return cmd
}

// update handles a key or a paste inside the form.
func (f *form) update(msg tea.Msg) tea.Cmd {
	cur := &f.fields[f.focus]
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "tab", "down":
			return f.focusField(f.focus + 1)
		case "shift+tab", "up":
			return f.focusField(f.focus - 1)
		}
		if cur.choices != nil {
			switch k.String() {
			case "right", "l", "space":
				cur.choice = (cur.choice + 1) % len(cur.choices)
			case "left", "h":
				cur.choice = (cur.choice + len(cur.choices) - 1) % len(cur.choices)
			}
			return nil
		}
	}
	if cur.choices != nil {
		return nil
	}
	f.err = nil
	var cmd tea.Cmd
	cur.input, cmd = cur.input.Update(msg)
	return cmd
}

// resize fits the inputs to the terminal width; f may be nil.
func (f *form) resize(width int) {
	if f == nil || width <= 0 {
		return
	}
	for i := range f.fields {
		f.fields[i].input.SetWidth(max(1, width-labelWidth-4))
	}
}
