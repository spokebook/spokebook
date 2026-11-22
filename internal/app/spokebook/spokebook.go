// Package spokebook wires up configuration, database and HTTP server.
package spokebook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
	"github.com/spokebook/spokebook/internal/app/spokebook/rides/garmin"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/server"
	"github.com/spokebook/spokebook/internal/app/spokebook/store/sqlite"
)

// Run starts the app and blocks until ctx is done.
func Run(ctx context.Context, log *slog.Logger) error {
	addr := env("APP_ADDR", "localhost:8080")
	dbPath := env("APP_DB_PATH", "data/dev.sqlite")
	baseURL := strings.TrimSuffix(env("APP_BASE_URL", "http://localhost:8080"), "/")
	proxies, err := server.ParseTrustedProxies(os.Getenv("APP_TRUSTED_PROXIES"))
	if err != nil {
		return err
	}
	allowPrivate, err := envBool("APP_NOTIFY_ALLOW_PRIVATE")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return err
	}

	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("close database", "err", err)
		}
	}()

	var box *secret.Box
	if key := os.Getenv("APP_SECRET_KEY"); key != "" {
		if box, err = secret.New(key); err != nil {
			return fmt.Errorf("APP_SECRET_KEY: %w", err)
		}
	}
	syncer := garmin.NewSyncer(st, box, garmin.NewClient(garmin.BaseURL), log)
	go syncer.Run(ctx)
	notifier := notify.New(st, box, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{ntfy.Kind(ntfy.NewClient(allowPrivate))}, Log: log,
	})
	go notifier.Run(ctx)

	version := AppVersion()
	app, err := server.New(ctx, st, log, server.Config{
		BaseURL: baseURL, TrustedProxies: proxies, Version: version, SourceURL: SourceURL,
		Garmin: syncer, Notify: notifier,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", "http://"+addr, "url", baseURL, "version", version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NewLogger writes text logs to w at the level in APP_LOG_LEVEL (debug, info,
// warn or error; default info).
func NewLogger(w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("APP_LOG_LEVEL", "info"))); err != nil {
		return nil, fmt.Errorf("APP_LOG_LEVEL: %w", err)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), nil
}

// envBool reads a boolean variable (true or false, 1 or 0, ...); unset
// is false.
func envBool(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: invalid value %q, want true or false", key, v)
	}
	return b, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
