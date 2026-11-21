package router

import (
	"net/http"
	"testing"
)

func TestIsSubagent(t *testing.T) {
	const marked = `[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322.f78; cc_entrypoint=sdk-cli; cc_is_subagent=true;"},{"type":"text","text":"sys"}]`
	withID := http.Header{"X-Claude-Code-Agent-Id": {"a1"}}
	cases := []struct {
		name   string
		h      http.Header
		system string
		want   bool
	}{
		{"marker and header", withID, marked, true},
		{"marker without header", http.Header{}, marked, false},
		{"header without marker (main thread)", withID, `[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322.f78; cc_entrypoint=sdk-cli;"}]`, false},
		{"marker false", withID, `[{"type":"text","text":"x-anthropic-billing-header: cc_is_subagent=false;"}]`, false},
		{"marker lookalike", withID, `[{"type":"text","text":"x-anthropic-billing-header: xcc_is_subagent=true;"}]`, false},
		{"marker not in first block", withID, `[{"type":"text","text":"sys"},{"type":"text","text":"x-anthropic-billing-header: cc_is_subagent=true;"}]`, false},
		{"marker in other text", withID, `[{"type":"text","text":"note cc_is_subagent=true;"}]`, false},
		{"string system", withID, `"x-anthropic-billing-header: cc_is_subagent=true;"`, false},
		{"no system", withID, ``, false},
		{"garbage", withID, `{`, false},
		{"no trailing semicolon", withID, `[{"type":"text","text":"x-anthropic-billing-header: cc_version=1; cc_is_subagent=true"}]`, true},
	}
	for _, c := range cases {
		if got := isSubagent(c.h, []byte(c.system)); got != c.want {
			t.Errorf("%s: isSubagent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFindCredential(t *testing.T) {
	clean := http.Header{"X-Api-Key": {"sk-deepseek"}}
	if w := findCredential(clean, []byte(`{"a":"sk-an"}`)); w != "" {
		t.Errorf("clean request flagged: %s", w)
	}
	for _, c := range []struct {
		h    http.Header
		body string
	}{
		{clean, `{"a":"SK-ANT-oat01"}`},
		{http.Header{"X-Foo": {"Bearer sk-ant-oat01-x"}}, `{}`},
		{http.Header{"Sk-Ant-Weird": {"v"}}, `{}`},
	} {
		if findCredential(c.h, []byte(c.body)) == "" {
			t.Errorf("missed credential in %v %s", c.h, c.body)
		}
	}
}
