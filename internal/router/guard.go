package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	billingPrefix   = "x-anthropic-billing-header:"
	subagentMarker  = "cc_is_subagent=true"
	agentIDHeader   = "X-Claude-Code-Agent-Id"
	credentialToken = "sk-ant-"
)

// isSubagent reports whether a request comes from a Claude Code subagent.
// Both signals observed in the spike must be present (fail closed):
// system[0] is the billing block carrying cc_is_subagent=true, and the
// x-claude-code-agent-id header is set.
func isSubagent(h http.Header, system json.RawMessage) bool {
	if h.Get(agentIDHeader) == "" {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(system, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	first := blocks[0]
	if first.Type != "text" || !strings.HasPrefix(first.Text, billingPrefix) {
		return false
	}
	for _, field := range strings.Split(strings.TrimPrefix(first.Text, billingPrefix), ";") {
		if strings.TrimSpace(field) == subagentMarker {
			return true
		}
	}
	return false
}

// findCredential returns where an Anthropic credential pattern appears in an
// outbound request, or "" if it is clean. Matching is case-insensitive.
func findCredential(h http.Header, body []byte) string {
	if bytes.Contains(bytes.ToLower(body), []byte(credentialToken)) {
		return "body"
	}
	for name, values := range h {
		if strings.Contains(strings.ToLower(name), credentialToken) {
			return "header name"
		}
		for _, v := range values {
			if strings.Contains(strings.ToLower(v), credentialToken) {
				return "header " + name
			}
		}
	}
	return ""
}
