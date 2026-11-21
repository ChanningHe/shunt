// Package router is shunt's request path: the passthrough to Anthropic, the
// sanitized hand-off of routed subagent requests to third-party providers,
// and the server that puts the admin API in front of both.
package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const (
	// AnthropicAPI is the only passthrough target. It is a constant, not
	// config, so nothing that can edit the config can redirect OAuth traffic.
	AnthropicAPI = "https://api.anthropic.com"

	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 5 * time.Second
)

// Serve serves the admin API and the proxy on ln until ctx is done.
// anthropic is the passthrough upstream: AnthropicAPI in production.
// The admin token is created only now that ln is bound, so a second
// instance that fails to bind never replaces a running daemon's token; it
// is removed on exit. A token that cannot be written leaves the admin API
// unusable but the router running.
func Serve(ctx context.Context, ln net.Listener, dir, anthropic string, store *config.Store, log *slog.Logger) error {
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return err
	}
	token, err := admin.NewToken()
	if err != nil {
		return err
	}
	if path, err := admin.WriteToken(dir, port, token); err != nil {
		log.Error("admin token not written; the admin API is unusable", "err", err)
	} else {
		defer os.Remove(path)
		log.Info("admin token written", "path", path)
	}
	adm := admin.New(store, token, ln.Addr().String(), log)
	px := newProxy(anthropic, store, log)
	px.rec, adm.Probe = adm.Activity, px.probe
	return serve(ctx, ln, withAdmin(adm, px), log)
}

// withAdmin hands the whole admin namespace to adm, before the proxy sees
// anything: no path in it may fall through to the passthrough, which would
// forward the admin token to Anthropic.
func withAdmin(adm, proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inAdminNamespace(r.URL.Path) {
			adm.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// inAdminNamespace claims a path whose raw or cleaned spelling, ASCII
// lower-cased, is in the namespace ("/_shunt/../x", "//_shunt/x",
// "/_SHUNT/x", "/a/../_shunt"): a hand-typed variant must get a 404 from
// admin, never a passthrough with the admin token attached.
func inAdminNamespace(p string) bool {
	owned := func(p string) bool {
		p = strings.Map(func(c rune) rune {
			if 'A' <= c && c <= 'Z' {
				return c + 'a' - 'A'
			}
			return c
		}, p)
		return p == admin.Prefix || strings.HasPrefix(p, admin.Prefix+"/")
	}
	return owned(p) || owned(path.Clean(p))
}

// NewLogger is the router's log format on w.
func NewLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

// OpenStore loads the config directory. Bad content never stops the router:
// it serves pure passthrough until the files are fixed and reloaded.
func OpenStore(dir string, log *slog.Logger) *config.Store {
	store := config.Open(dir)
	snap := store.Load()
	if snap.LoadErr != nil {
		log.Error("config not loaded; passthrough only until fixed and reloaded", "dir", dir, "err", snap.LoadErr)
		return store
	}
	log.Info("config loaded", "dir", dir, "providers", len(snap.Config.Providers), "routes", len(snap.Config.Routes))
	return store
}

// ReloadOn re-reads the config directory on every signal until ctx is done.
// A failed reload keeps the running config.
func ReloadOn(ctx context.Context, sig <-chan os.Signal, store *config.Store, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
		}
		if err := store.Reload(); err != nil {
			log.Error("config reload failed; keeping the running config", "err", err)
			continue
		}
		snap := store.Load()
		log.Info("config reloaded", "providers", len(snap.Config.Providers), "routes", len(snap.Config.Routes))
	}
}

// serve runs h on ln until ctx is done, then drains in-flight requests.
func serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		drained <- srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown starts; wait for in-flight streams
	// to finish (or the timeout) before exiting.
	if err := <-drained; err != nil {
		log.Warn("shutdown timed out; in-flight requests cut off", "err", err)
	}
	return nil
}
