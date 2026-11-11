// Command tasmonaut serves a small fleet console for Tasmota devices: an
// in-memory inventory built from configuration and network discovery, status
// polling, power and console commands, configuration download and restore,
// and firmware upgrades, singly or in bulk. It keeps no state on disk.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rake-pro/tasmonaut/internal/config"
	"github.com/rake-pro/tasmonaut/internal/fleet"
	"github.com/rake-pro/tasmonaut/internal/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to a YAML config file (env TASMONAUT_CONFIG)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("tasmonaut", version)
		return
	}

	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("configuration")
	}
	if lvl, err := zerolog.ParseLevel(strings.ToLower(cfg.LogLevel)); err == nil {
		zerolog.SetGlobalLevel(lvl)
	} else {
		log.Warn().Str("logLevel", cfg.LogLevel).Msg("unknown log level, using info")
	}

	if err := run(cfg); err != nil {
		log.Fatal().Err(err).Msg("tasmonaut exited with error")
	}
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info().Str("version", version).Str("listen", cfg.Listen).Int("configured", len(cfg.Devices)).
		Strs("discovery", cfg.Discovery.CIDRs).Msg("tasmonaut starting")

	log.Warn().Str("identityHeader", cfg.IdentityHeader).
		Msg("tasmonaut has no login of its own: run it behind an authenticating reverse proxy (SSO gate); every request that reaches the listener is trusted")

	fl := fleet.New(cfg)
	fl.Start(ctx)

	handler, err := server.New(fl, version, cfg.IdentityHeader)
	if err != nil {
		return err
	}
	servers := []*http.Server{{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Long enough for a bulk config download or a sweep-sized refresh.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}}
	if m := cfg.Firmware.Mirror; m.Listen != "" {
		log.Info().Str("listen", m.Listen).Str("url", m.URL).
			Msg("firmware mirror on: plain HTTP, no login, serves only /firmware/<image>; expose it to the device network only")
		servers = append(servers, &http.Server{
			Addr:              m.Listen,
			Handler:           server.NewMirror(fl),
			ReadHeaderTimeout: 10 * time.Second,
			// A 2 MB ESP32 image over a weak Wi-Fi link takes a while.
			WriteTimeout:   5 * time.Minute,
			IdleTimeout:    time.Minute,
			MaxHeaderBytes: 16 << 10,
		})
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			log.Info().Str("addr", srv.Addr).Msg("listening")
			errc <- srv.ListenAndServe()
		}()
	}
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info().Msg("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var errs []error
		for _, srv := range servers {
			errs = append(errs, srv.Shutdown(shutdownCtx))
		}
		return errors.Join(errs...)
	}
	return nil
}
