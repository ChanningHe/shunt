package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const (
	probeVersion    = "2023-06-01" // anthropic-version of the test request
	probeMaxTokens  = 16
	maxProbeBody    = 8 << 10
	maxProbeMessage = 300
)

// probe sends one minimal request through the named route, built like a
// routed request (same headers, same credential guard), and reports what the
// provider answered. It is how the admin API tests a route.
func (p *proxy) probe(ctx context.Context, name string) admin.TestResult {
	snap := p.store.Load()
	_, route, prov, _, ok := snap.Config.Match(config.WirePrefix + name)
	if !ok {
		return admin.TestResult{Message: fmt.Sprintf("route %s has no usable provider", name)}
	}
	key, state := p.store.Key(snap, route.Provider)
	if key == "" {
		return admin.TestResult{Message: fmt.Sprintf("key for provider %s is %s", route.Provider, state)}
	}
	body, _ := json.Marshal(map[string]any{
		"model":      route.Model,
		"max_tokens": probeMaxTokens,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	h := outboundHeaders(http.Header{"Anthropic-Version": {probeVersion}}, profile(prov), key)
	if where := findCredential(h, body); where != "" {
		return admin.TestResult{Message: "refused: the request contains an Anthropic credential pattern in " + where}
	}
	ctx, cancel := context.WithTimeout(ctx, admin.ProbeTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, prov.BaseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return admin.TestResult{Message: "cannot build the request"}
	}
	out.Header = h

	start := time.Now()
	resp, err := p.client.Do(out)
	res := admin.TestResult{Ms: time.Since(start).Milliseconds()}
	if err != nil {
		res.Message = out.URL.Host + " unreachable"
		if errors.Is(err, context.DeadlineExceeded) {
			res.Message = fmt.Sprintf("%s did not answer within %s", out.URL.Host, admin.ProbeTimeout)
		}
		return res
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	res.Ms = time.Since(start).Milliseconds()
	res.Status = resp.StatusCode
	if res.OK = resp.StatusCode/100 == 2; !res.OK {
		res.Message = probeMessage(data, key)
	}
	return res
}

// probeMessage extracts the provider's error text from an untrusted body:
// the Anthropic error shape if it parses, else the raw start of the body.
// The key is masked, control characters are dropped, the length is bounded.
func probeMessage(data []byte, key string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := string(data)
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	return admin.Printable(strings.ReplaceAll(msg, key, "<key>"), maxProbeMessage)
}
