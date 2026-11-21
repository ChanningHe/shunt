package router

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func sanitizeFixture(t *testing.T) (object, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/subagent_request.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := sanitize(deepseekProfile, "deepseek-flash", raw)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var obj object
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("sanitized output is not JSON: %v", err)
	}
	return obj, out
}

func TestSanitizeTopLevel(t *testing.T) {
	obj, _ := sanitizeFixture(t)
	if obj["model"] != "deepseek-flash" {
		t.Errorf("model = %v, want deepseek-flash", obj["model"])
	}
	for _, k := range []string{"metadata", "safeguards", "context_management"} {
		if _, ok := obj[k]; ok {
			t.Errorf("%s must be dropped", k)
		}
	}
	if got := obj["output_config"]; !reflect.DeepEqual(got, object{"effort": "high"}) {
		t.Errorf("output_config = %v, want only effort (task_budget dropped)", got)
	}
	if got := obj["thinking"]; !reflect.DeepEqual(got, object{"type": "enabled"}) {
		t.Errorf("thinking = %v, want {type: enabled}", got)
	}
	for _, k := range []string{"max_tokens", "stream", "temperature"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s must be kept", k)
		}
	}
}

func TestSanitizeSystemDropsBillingBlock(t *testing.T) {
	obj, _ := sanitizeFixture(t)
	want := []any{
		object{"type": "text", "text": "You are a Claude agent."},
		object{"type": "text", "text": "You are an agent for Claude Code."},
	}
	if !reflect.DeepEqual(obj["system"], want) {
		t.Errorf("system = %v, want %v", obj["system"], want)
	}
}

func TestSanitizeTools(t *testing.T) {
	obj, _ := sanitizeFixture(t)
	var names []string
	for _, tool := range obj["tools"].([]any) {
		m := tool.(object)
		names = append(names, m["name"].(string))
		for k := range m {
			if k != "name" && k != "description" && k != "input_schema" {
				t.Errorf("tool %s keeps key %q", m["name"], k)
			}
		}
	}
	want := []string{"Read", "Bash", "SubagentHandback", "mcp__docs__guide"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tools = %v, want %v (server/deferred dropped, tool_addition hoisted)", names, want)
	}
	// A schema property that happens to be named cache_control must survive.
	props := obj["tools"].([]any)[0].(object)["input_schema"].(object)["properties"].(object)
	if _, ok := props["cache_control"]; !ok {
		t.Error("input_schema property named cache_control was stripped")
	}
}

func TestSanitizeMessages(t *testing.T) {
	obj, _ := sanitizeFixture(t)
	msgs := obj["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.(object)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user"}) {
		t.Fatalf("roles = %v, want user/assistant/user (system messages folded)", roles)
	}
	first := msgs[0].(object)["content"].([]any)
	if last := first[len(first)-1].(object)["text"]; last != "The following tools just became available" {
		t.Errorf("system text after a user message must be appended to it, got last block %v", last)
	}
	var asstTypes []string
	for _, b := range msgs[1].(object)["content"].([]any) {
		asstTypes = append(asstTypes, b.(object)["type"].(string))
	}
	if !reflect.DeepEqual(asstTypes, []string{"thinking", "tool_use"}) {
		t.Errorf("assistant blocks = %v, want thinking+tool_use (redacted_thinking, server tool blocks dropped)", asstTypes)
	}
	toolUse := msgs[1].(object)["content"].([]any)[1].(object)
	if toolUse["input"].(object)["cache_control"] != "keep-me" {
		t.Error("tool_use input content must not be touched")
	}
	last := msgs[2].(object)["content"].([]any)
	if last[0].(object)["type"] != "tool_result" {
		t.Errorf("tool_result must stay first in its user message, got %v", last[0])
	}
	if last[len(last)-1].(object)["text"] != "A tool changed." {
		t.Errorf("system text after an assistant message must go to the end of the next user message")
	}
	nested := last[0].(object)["content"].([]any)
	if len(nested) != 1 || nested[0].(object)["type"] != "text" {
		t.Errorf("tool_result nested content = %v, want only the text block", nested)
	}
}

func TestSanitizeStripsCacheControl(t *testing.T) {
	obj, _ := sanitizeFixture(t)
	for _, k := range []string{"system", "messages"} {
		b, _ := json.Marshal(obj[k])
		if strings.Contains(string(b), `"cache_control":{`) {
			t.Errorf("%s still contains cache_control: %s", k, b)
		}
	}
	for _, tool := range obj["tools"].([]any) {
		if _, ok := tool.(object)["cache_control"]; ok {
			t.Errorf("tool %v keeps cache_control", tool.(object)["name"])
		}
	}
}

func TestSanitizeThinkingAndToolChoice(t *testing.T) {
	base := `{"model":"ds/x","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"A","input_schema":{}}],`
	cases := []struct {
		name, extra    string
		thinking       any
		keepToolChoice bool
	}{
		{"adaptive", `"thinking":{"type":"adaptive"}`, object{"type": "enabled"}, false},
		{"enabled budget", `"thinking":{"type":"enabled","budget_tokens":999}`, object{"type": "enabled"}, false},
		{"disabled", `"thinking":{"type":"disabled"}`, object{"type": "disabled"}, false},
		{"unknown", `"thinking":{"type":"weird"}`, nil, false},
		{"tool_choice any", `"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}`, object{"type": "disabled"}, true},
		{"tool_choice named", `"tool_choice":{"type":"tool","name":"A"}`, object{"type": "disabled"}, true},
		{"tool_choice dropped tool", `"thinking":{"type":"adaptive"},"tool_choice":{"type":"tool","name":"Gone"}`, object{"type": "enabled"}, false},
		{"tool_choice auto", `"tool_choice":{"type":"auto"}`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := sanitize(deepseekProfile, "m", []byte(base+c.extra+"}"))
			if err != nil {
				t.Fatal(err)
			}
			var obj object
			_ = json.Unmarshal(out, &obj)
			if !reflect.DeepEqual(obj["thinking"], c.thinking) {
				t.Errorf("thinking = %v, want %v", obj["thinking"], c.thinking)
			}
			if _, ok := obj["tool_choice"]; ok != c.keepToolChoice {
				t.Errorf("tool_choice kept = %v, want %v", ok, c.keepToolChoice)
			}
		})
	}
}

func TestSanitizePreservesNumbersAndHTML(t *testing.T) {
	in := `{"model":"ds/x","max_tokens":12345678901234567890,"messages":[{"role":"user","content":"a <b> & c"}]}`
	out, err := sanitize(deepseekProfile, "m", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "12345678901234567890") || !strings.Contains(s, "a <b> & c") {
		t.Errorf("numbers/HTML altered: %s", s)
	}
}

func TestSanitizeRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		``, `null`, `[]`, `{"messages":"x"}`, `{"messages":[1]}`,
		`{"messages":[{"role":"tool","content":"x"}]}`, `{"messages":[{"role":"user","content":1}]}`,
		`{"messages":[],"system":1}`, `{"messages":[],"tools":{}}`, `{"messages":[],"tools":[{"input_schema":{}}]}`,
		`{"messages":[]} {"x":1}`,
	} {
		if _, err := sanitize(deepseekProfile, "m", []byte(in)); err == nil {
			t.Errorf("sanitize(%q) = nil error, want error", in)
		}
	}
}

func FuzzSanitizeNoPanic(f *testing.F) {
	raw, _ := os.ReadFile("testdata/subagent_request.json")
	f.Add(raw)
	f.Add([]byte(`{"messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":1}}}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = sanitize(deepseekProfile, "m", body)
	})
}

// Interactive sessions carry tools (e.g. Artifact) whose schemas use regex
// "pattern" keywords such as "^[^\\0]*$"; DeepSeek rejects the whole request
// ("is not a regex"). Regex keywords are dropped; a property that is merely
// named "pattern" (Grep) must survive.
func TestSanitizeDropsSchemaRegexKeywords(t *testing.T) {
	in := `{"model":"ds/x","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Artifact","input_schema":{
		"type":"object",
		"properties":{
			"pattern":{"type":"string","description":"grep regex","pattern":"^[^\\0]*$"},
			"file_paths":{"type":"array","items":{"type":"string","pattern":"^[^\\0]*$"}},
			"choice":{"anyOf":[{"type":"string","pattern":"^a"},{"type":"null"}]},
			"nested":{"type":"object","properties":{"p":{"type":"string","pattern":"x"}},"patternProperties":{"^x-":{"type":"string"}}}
		},
		"$defs":{"d":{"type":"string","pattern":"y"}},
		"dependencies":{"x":{"properties":{"q":{"type":"string","pattern":"z"}}},"y":["x"]},
		"contentSchema":{"type":"string","pattern":"w"}
	}}]}`
	out, err := sanitize(deepseekProfile, "m", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var obj object
	_ = json.Unmarshal(out, &obj)
	schema, _ := json.Marshal(obj["tools"].([]any)[0].(object)["input_schema"])
	s := string(schema)
	if strings.Contains(s, `"pattern":"`) || strings.Contains(s, "patternProperties") {
		t.Errorf("regex keywords left in schema: %s", s)
	}
	if !strings.Contains(s, `"pattern":{"description":"grep regex","type":"string"}`) {
		t.Errorf("property named pattern was damaged: %s", s)
	}
}
