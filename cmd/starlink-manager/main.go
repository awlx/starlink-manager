// Command starlink-manager serves the FFMUC Starlink manager web app.
//
// It polls the Starlink account API (the API the Starlink app/site uses)
// for all dishes and routers on the account, caches device status and
// history locally, and exposes a dashboard plus admin actions behind
// Keycloak (or forward-auth proxy) authentication.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/awlx/starlink-manager/internal/auth"
	"github.com/awlx/starlink-manager/internal/config"
	"github.com/awlx/starlink-manager/internal/starlink"
	"github.com/awlx/starlink-manager/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "starlink-manager: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Auth client carries the cookie jar (probe + token renewal); data
	// endpoints use a separate cookie-free client (Bearer-only).
	client := &http.Client{Timeout: 2 * cfg.CallTimeout}
	session := starlink.NewSession(cfg.StarlinkAPIBaseURL, cfg.StarlinkWebBaseURL,
		cfg.StarlinkSSOCookie, client, log)
	if cfg.StarlinkSessionToken != "" {
		session.WithPresetToken(cfg.StarlinkSessionToken)
		log.Info("using pre-minted access token override")
	}
	if cfg.StarlinkRefreshCookies != "" {
		session.WithRefreshCookies(cfg.StarlinkRefreshCookies)
		log.Info("seeded session cookies for token renewal")
	}
	if cfg.StarlinkAccountNumber != "" {
		session.WithAccountNumber(cfg.StarlinkAccountNumber)
		log.Info("using configured account number", "account", cfg.StarlinkAccountNumber)
	}
	gateway := starlink.NewGateway(session, cfg.StarlinkWebBaseURL, session.DataClient(), log)
	gateway.SetCallTimeout(cfg.CallTimeout)
	roster := starlink.NewRoster(session, cfg.StarlinkAPIBaseURL, cfg.RosterCacheTTL, log)
	telemetry := starlink.NewTelemetry(session, cfg.StarlinkAPIBaseURL, log)

	store := starlink.NewStore(cfg.DataDir, cfg.HistoryWindow, nil)
	if err := store.Load(); err != nil {
		return fmt.Errorf("load persisted history from %s: %w", cfg.DataDir, err)
	}

	poller := starlink.NewPoller(gateway, roster, telemetry, session, store, cfg.PollInterval, log)
	poller.Start()
	defer poller.Stop()

	authN, err := buildAuth(&cfg, log)
	if err != nil {
		return fmt.Errorf("authentication setup: %w", err)
	}

	srv, err := web.New(&cfg, authN, gateway, roster, store, session)
	if err != nil {
		return fmt.Errorf("web setup: %w", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "auth_mode", cfg.AuthMode,
			"poll_interval", cfg.PollInterval, "data_dir", cfg.DataDir)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "err", err)
		}
		// Persist history one last time on shutdown.
		if err := store.Save(); err != nil {
			log.Error("saving history on shutdown failed", "err", err)
		}
		return nil
	}
}

// buildAuth constructs the authenticator for the configured AUTH_MODE.
func buildAuth(cfg *config.Config, log *slog.Logger) (auth.Authenticator, error) {
	switch cfg.AuthMode {
	case config.AuthModeOIDC:
		return auth.NewOIDCAuth(context.Background(),
			cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL,
			cfg.OIDCAdminClaim, cfg.OIDCAdminGroups,
			cfg.SessionCookieSecret, cfg.SessionCookieSecure, log)
	case config.AuthModeForwardAuth:
		return &auth.ForwardAuth{
			Headers: auth.HeaderNames{
				User:   cfg.ForwardAuthHeaders.User,
				Email:  cfg.ForwardAuthHeaders.Email,
				Groups: cfg.ForwardAuthHeaders.Groups,
			},
			Admin: auth.AdminMatcher{ClaimName: "", Groups: cfg.OIDCAdminGroups},
		}, nil
	case config.AuthModeLocalDev:
		if !cfg.LocalDevUser {
			return nil, errors.New("AUTH_MODE=localdev requires LOCAL_DEV_USER=true")
		}
		return auth.LocalDev{}, nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q", cfg.AuthMode)
	}
}
