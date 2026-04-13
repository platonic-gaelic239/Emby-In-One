package backend

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// App is the main application struct holding all shared dependencies.
type App struct {
	Version         string
	ConfigStore     *ConfigStore
	Logger          *Logger
	IDStore         *IDStore
	Identity        *ClientIdentityService
	Auth            *AuthManager
	Upstream        *UpstreamPool
	UserStore       *UserStore
	WatchStore      *WatchStore
	PlaybackLimiter *PlaybackLimiter
	loginLimiter    loginRateLimiter
}

func NewApp() (*App, error) {
	configStore, err := LoadConfigStore()
	if err != nil {
		return nil, err
	}
	cfg := configStore.Snapshot()
	logger := NewLogger(LogConfig{DataDir: cfg.DataDir})
	idStore, err := NewIDStore(cfg.DataDir, logger)
	if err != nil {
		return nil, err
	}
	identity := NewClientIdentityServiceFromDetectedConfig()
	auth, err := NewAuthManager(configStore, identity, logger)
	if err != nil {
		return nil, err
	}
	upstream := NewUpstreamPool(cfg, logger)
	upstream.LoginAll()

	var userStore *UserStore
	var watchStore *WatchStore
	if db := idStore.DB(); db != nil {
		us, err := NewUserStore(db, logger)
		if err != nil {
			logger.Warnf("UserStore init failed: %v (multi-user disabled)", err)
		} else {
			userStore = us
		}
		wst, err := NewWatchStore(db, logger)
		if err != nil {
			logger.Warnf("WatchStore init failed: %v (per-user watch history disabled)", err)
		} else {
			watchStore = wst
		}
	}

	return &App{
		ConfigStore:     configStore,
		Logger:          logger,
		IDStore:         idStore,
		Identity:        identity,
		Auth:            auth,
		Upstream:        upstream,
		UserStore:       userStore,
		WatchStore:      watchStore,
		PlaybackLimiter: NewPlaybackLimiter(),
	}, nil
}

func (a *App) Close() error {
	if a.Upstream != nil {
		a.Upstream.stopHealthChecks()
	}
	if a.IDStore != nil {
		_ = a.IDStore.Close()
	}
	if a.Logger != nil {
		_ = a.Logger.Close()
	}
	return nil
}

func (a *App) Run() error {
	cfg := a.ConfigStore.Snapshot()
	server := &http.Server{
		Addr:              ":" + intToString(cfg.Server.Port),
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start periodic stream URL eviction
	evictCtx, evictCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-evictCtx.Done():
				return
			case <-ticker.C:
				a.IDStore.evictExpiredStreamURLs()
				a.loginLimiter.cleanup()
				if a.PlaybackLimiter != nil {
					a.PlaybackLimiter.Cleanup()
				}
			}
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdownCh
		a.Logger.Infof("Shutdown signal received, draining connections...")
		evictCancel()
		a.Upstream.stopHealthChecks()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	a.Logger.Infof("Go Emby-in-One listening on port %d", cfg.Server.Port)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
