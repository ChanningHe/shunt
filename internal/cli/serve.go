package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"shunt/internal/config"
	"shunt/internal/router"
)

func cmdServe(stdout, _ io.Writer, args []string) error {
	dir, listen := config.DefaultDir(), ""
	opts := []opt{
		configDirOpt(&dir),
		stringOpt('l', "listen", "ADDR", "listen address for this process only, loopback only; never written to the config "+
			"(default: the config's listen, else "+config.DefaultListen+")", &listen),
	}
	rest, err := parseCmd(stdout, "serve", "[options]", opts, args)
	if err != nil {
		return quietHelp(err)
	}
	if err := noPositional(rest); err != nil {
		return err
	}
	if dir == "" {
		return errors.New("cannot determine the config directory; pass --config-dir")
	}
	if err := (config.Config{Listen: listen}).Validate(); err != nil {
		return err
	}
	// Registered before anything else: an unhandled SIGHUP kills the process.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	log := router.NewLogger(os.Stderr)
	store := router.OpenStore(dir, log)
	if listen == "" {
		listen = store.Load().Config.ListenAddr()
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go router.ReloadOn(ctx, hup, store, log)
	return router.Serve(ctx, ln, dir, router.AnthropicAPI, store, log)
}
