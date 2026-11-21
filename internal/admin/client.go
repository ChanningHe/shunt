package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shunt/internal/config"
)

const (
	clientTimeout = 10 * time.Second
	// ProbeTimeout bounds the request a route test sends to the provider;
	// the client waits a little longer for the daemon's answer.
	ProbeTimeout     = 20 * time.Second
	testTimeout      = ProbeTimeout + 5*time.Second
	maxResponseBytes = 1 << 20
)

// Client is the one Go client of the admin API.
type Client struct {
	url        string // http://host:port, no trailing slash
	token      string
	configPath string // for the "edit it by hand" hint
	hc         *http.Client
}

// Error is a non-2xx answer of the daemon.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (HTTP %d %s)", e.Message, e.Status, e.Code) }

// DaemonURL is override when set (it must pass CheckURL), else http:// plus
// the listen address of the config in configDir, read without writing.
func DaemonURL(configDir, override string) (string, error) {
	base, _, err := locate(configDir, override)
	return base, err
}

// locate returns the daemon URL and the config file path.
func locate(configDir, override string) (base, configPath string, err error) {
	if configDir == "" { // also the token's fallback location
		return "", "", errors.New("cannot determine the config directory; pass --config-dir")
	}
	snap := config.Open(configDir).Load()
	configPath = snap.Files[0].Path
	if override == "" {
		return "http://" + snap.Config.ListenAddr(), configPath, nil
	}
	if err := CheckURL(override); err != nil {
		return "", "", err
	}
	return strings.TrimSuffix(override, "/"), configPath, nil
}

// Dial locates the daemon (see DaemonURL) and reads its admin token from
// TokenPath. It does not contact the daemon.
func Dial(configDir, override string) (*Client, error) {
	base, configPath, err := locate(configDir, override)
	if err != nil {
		return nil, err
	}
	c := &Client{url: base, configPath: configPath, hc: &http.Client{}}
	u, _ := url.Parse(base) // checked by locate
	port := u.Port()
	if port == "" {
		port = "80"
	}
	path := TokenPath(configDir, port)
	if c.token, err = readToken(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("no admin token at %s", path)
		}
		return nil, c.down(err)
	}
	return c, nil
}

// URL is the daemon's base URL.
func (c *Client) URL() string { return c.url }

// down explains how to get going when the daemon cannot be reached.
func (c *Client) down(cause error) error {
	hint := "start it with: systemctl --user start shunt (or shunt serve)"
	if c.configPath != "" {
		hint += "; or edit " + c.configPath + " by hand, which the daemon reads when it starts"
	}
	return fmt.Errorf("daemon not reachable at %s (%v)\n%s", c.url, cause, hint)
}

func (c *Client) State() (*State, error) {
	var st State
	return &st, c.do(http.MethodGet, "state", nil, &st)
}

// PutProvider creates the provider or changes its base_url, keeping
// hand-written fields. keyCleared reports that a literal key was dropped
// because the host changed.
func (c *Client) PutProvider(name, baseURL string) (keyCleared bool, err error) {
	var out struct {
		KeyCleared bool `json:"key_cleared"`
	}
	err = c.do(http.MethodPut, "providers/"+url.PathEscape(name), map[string]string{"base_url": baseURL}, &out)
	return out.KeyCleared, err
}

func (c *Client) DeleteProvider(name string) error {
	return c.do(http.MethodDelete, "providers/"+url.PathEscape(name), nil, nil)
}

// SetKey stores a literal key or a file:///abs/path reference and returns
// the resulting key state.
func (c *Client) SetKey(name, value string) (state string, err error) {
	var out struct {
		State string `json:"state"`
	}
	err = c.do(http.MethodPut, "providers/"+url.PathEscape(name)+"/key", map[string]string{"value": value}, &out)
	return out.State, err
}

func (c *Client) DeleteKey(name string) error {
	return c.do(http.MethodDelete, "providers/"+url.PathEscape(name)+"/key", nil, nil)
}

func (c *Client) PutRoute(name, provider, model string) (RouteResult, error) {
	var out RouteResult
	err := c.do(http.MethodPut, "routes/"+url.PathEscape(name), config.Route{Provider: provider, Model: model}, &out)
	return out, err
}

func (c *Client) DeleteRoute(name string) (RouteResult, error) {
	var out RouteResult
	err := c.do(http.MethodDelete, "routes/"+url.PathEscape(name), nil, &out)
	return out, err
}

// TestRoute sends one minimal request through the route (see TestResult).
func (c *Client) TestRoute(name string) (TestResult, error) {
	var out TestResult
	err := c.call(testTimeout, http.MethodPost, "routes/"+url.PathEscape(name)+"/test", nil, &out)
	return out, err
}

func (c *Client) Activity() (*Activity, error) {
	var a Activity
	return &a, c.do(http.MethodGet, "activity", nil, &a)
}

func (c *Client) Reload() error { return c.do(http.MethodPost, "reload", nil, nil) }

func (c *Client) WebCode() (WebCode, error) {
	var out WebCode
	err := c.do(http.MethodPost, "web-code", nil, &out)
	return out, err
}

func (c *Client) do(method, call string, in, out any) error {
	return c.call(clientTimeout, method, call, in, out)
}

func (c *Client) call(timeout time.Duration, method, call string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+apiPrefix+call, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if errors.Is(err, context.DeadlineExceeded) { // it is up, but slow: not "start the daemon"
		return fmt.Errorf("the daemon at %s did not answer within %s", c.url, timeout)
	}
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the *url.Error would repeat the URL
		}
		return c.down(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error.Message == "" {
			e.Error.Message = http.StatusText(resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("bad response from %s: %w", c.url, err)
	}
	return nil
}
