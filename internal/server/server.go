// Package server serves the local JSON API and resumable event streams.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/web"
)

// Options supplies trusted infrastructure. Zero values use production defaults.
type Options struct {
	Token           secret.Secret
	Runner          runner.Options
	ShutdownTimeout time.Duration
	// Short intervals are useful to exercise timers without network or live keys.
	HeartbeatInterval  time.Duration
	PollInterval       time.Duration
	StreamWriteTimeout time.Duration
	SyncInterval       time.Duration
}

// Server owns background runs and stream lifetimes. The caller owns the store
// and closes it only after Shutdown and outstanding runner cleanup finish.
type Server struct {
	cfg             *config.Config
	db              store.Store
	runner          *runner.Runner
	models          *llm.ModelLister
	registry        *llm.Registry
	redactor        *secret.Redactor
	manager         *manager
	bus             broadcaster
	handler         http.Handler
	http            *http.Server
	tokenHash       [32]byte
	authenticated   bool
	opts            Options
	mu              sync.RWMutex
	addr            string
	stopping        bool
	stop            context.CancelFunc
	done            chan struct{}
	maintenanceDone chan struct{}
	unsubscribe     func()
	shutdownOnce    sync.Once
}

// ValidateAddress refuses public or wildcard binds without a UI credential.
// Hostnames other than localhost require authentication: no DNS lookup can
// change an unauthenticated listener from loopback to a public interface.
func ValidateAddress(addr string, token secret.Secret) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("server address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("server port must be between 0 and 65535")
	}
	ip := net.ParseIP(host)
	if !token.IsSet() && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("non-loopback server address requires PATCHBOT_UI_TOKEN")
	}
	return nil
}

// New constructs one runner and starts lifecycle maintenance and ownership heartbeats.
func New(ctx context.Context, cfg *config.Config, db store.Store, opts Options) (*Server, error) {
	if cfg == nil || db == nil {
		return nil, errors.New("server requires configuration and storage")
	}
	if !opts.Token.IsSet() {
		opts.Token = secret.New(os.Getenv("PATCHBOT_UI_TOKEN"))
	}
	if err := ValidateAddress(cfg.Server.Addr, opts.Token); err != nil {
		return nil, err
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	if opts.StreamWriteTimeout <= 0 {
		opts.StreamWriteTimeout = 5 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = 60 * time.Second
	}
	if opts.Runner.Constructors == nil {
		opts.Runner.Constructors = providers.Constructors(providers.Options{})
	}
	s := &Server{cfg: cfg, db: db, opts: opts, addr: cfg.Server.Addr, manager: newManager(cfg.Server.MaxConcurrentRuns, cfg.Server.MaxQueuedRuns), redactor: cfg.Redactor(), done: make(chan struct{}), maintenanceDone: make(chan struct{})}
	s.authenticated = opts.Token.IsSet()
	s.tokenHash = sha256.Sum256([]byte(opts.Token.Reveal()))
	s.redactor.AddExact(opts.Token.Reveal())
	s.redactor.AddSecrets(secret.New(os.Getenv("GITHUB_TOKEN")))
	if opts.Runner.Tokens != nil {
		if value, err := opts.Runner.Tokens.Token(ctx); err == nil {
			s.redactor.AddExact(value)
		}
	}
	if source, ok := db.(interface {
		SubscribeEvents(func(store.Event)) func()
	}); ok {
		s.unsubscribe = source.SubscribeEvents(func(event store.Event) { s.bus.publish(event.RunID) })
	}
	opts.Runner.Dispatch = s.manager.submit
	opts.Runner.Admit = s.manager.reserve
	opts.Runner.ActionChannel = "ui"
	s.redactor.Extend(opts.Runner.Redactor)
	opts.Runner.Redactor = s.redactor
	var err error
	s.runner, err = runner.New(ctx, cfg, db, opts.Runner)
	if err != nil {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		return nil, errors.New("could not initialize runner")
	}
	configs, err := cfg.LLMProviders()
	if err != nil {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		return nil, errors.New("invalid provider configuration")
	}
	s.registry, err = llm.NewRegistry(opts.Runner.Constructors, configs, llm.RegistryOptions{})
	if err != nil {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		return nil, errors.New("could not initialize providers")
	}
	s.models = llm.NewModelLister(s.registry, nil)
	s.handler = s.routes()
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	maintenanceCtx, stop := context.WithCancel(context.Background())
	s.stop = stop
	interval := opts.Runner.HeartbeatInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	s.manager.startHeartbeats(interval, db.Heartbeat)
	go s.maintain(maintenanceCtx)
	return s, nil
}

func (s *Server) maintain(ctx context.Context) {
	defer close(s.maintenanceDone)
	ticker := time.NewTicker(s.opts.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.runner.RecoverStale(ctx)
			_ = s.runner.SyncWaiting(ctx)
		}
	}
}

// Serve runs HTTP until the caller cancels ctx or Shutdown is called. The
// listener must be bound to the configured address; ListenAndServe binds it.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if err := ValidateAddress(listener.Addr().String(), s.opts.Token); err != nil {
		_ = listener.Close()
		return err
	}
	s.mu.Lock()
	s.addr = listener.Addr().String()
	s.mu.Unlock()
	finished := make(chan struct{})
	type shutdownResult struct {
		called bool
		err    error
	}
	watcherDone := make(chan shutdownResult, 1)
	go func() {
		select {
		case <-ctx.Done():
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
			defer stop()
			watcherDone <- shutdownResult{called: true, err: s.Shutdown(cleanup)}
		case <-finished:
			watcherDone <- shutdownResult{}
		}
	}()
	err := s.http.Serve(listener)
	close(finished)
	result := <-watcherDone
	if result.called {
		if result.err != nil {
			return result.err
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer stop()
	shutdownErr := s.Shutdown(cleanup)
	if shutdownErr != nil {
		return shutdownErr
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ListenAndServe binds the configured address and handles graceful shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Server.Addr)
	if err != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
		defer stop()
		_ = s.Shutdown(cleanup)
		return errors.New("could not bind server address")
	}
	return s.Serve(ctx, listener)
}

// Shutdown rejects new work, stops maintenance and SSE, and drains in-flight
// runs up to ctx's deadline. On timeout it cancels their execution contexts.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		s.manager.stopAdmission()
		s.stop()
		close(s.done)
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
	})
	httpErr := s.http.Shutdown(ctx)
	workErr := s.manager.wait(ctx)
	if httpErr != nil {
		_ = s.http.Close()
	}
	select {
	case <-s.maintenanceDone:
	case <-ctx.Done():
		return errors.Join(httpErr, workErr, ctx.Err())
	}
	return errors.Join(httpErr, workErr)
}

// ServeHTTP applies authentication and security to every route and error.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", web.CSP(s.cfg.Server.VueSrc == "local"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	// Only data-free shells and their fixed assets are public. Browser navigation
	// cannot attach a bearer header; every API and SSE request still requires it.
	if s.authenticated && (r.Method != http.MethodGet || !web.PublicPath(r.URL.Path)) {
		value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		candidate := sha256.Sum256([]byte(value))
		if !ok || subtle.ConstantTimeCompare(candidate[:], s.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			s.writeError(w, http.StatusUnauthorized, "unauthorized", "bearer token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		if err := http.NewCrossOriginProtection().Check(r); err != nil {
			s.writeError(w, http.StatusForbidden, "forbidden", "cross-origin request refused")
			return
		}
	}
	if !s.validHost(r) {
		s.writeError(w, http.StatusForbidden, "forbidden", "request host refused")
		return
	}
	s.mu.RLock()
	stopping := s.stopping
	s.mu.RUnlock()
	if stopping {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "server is shutting down")
		return
	}
	if r.URL.Path != path.Clean(r.URL.Path) {
		s.writeError(w, 404, "not_found", "resource not found")
		return
	}
	s.handler.ServeHTTP(w, r)
}

func (s *Server) validHost(r *http.Request) bool {
	s.mu.RLock()
	addr := s.addr
	s.mu.RUnlock()
	boundHost, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host, requestPort, err := net.SplitHostPort(r.Host)
	if err != nil {
		// Browsers omit the port when the listener uses HTTP's default port.
		host = r.Host
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
			if net.ParseIP(host) == nil {
				return false
			}
		} else if strings.Contains(host, ":") {
			return false
		}
		requestPort = "80"
		if r.TLS != nil {
			requestPort = "443"
		}
	}
	if host == "" || requestPort != port {
		return false
	}
	ip := net.ParseIP(boundHost)
	requestIP := net.ParseIP(host)
	if ip != nil && !ip.IsUnspecified() && requestIP != nil && ip.Equal(requestIP) {
		return true
	}
	loopback := (ip != nil && ip.IsLoopback()) || strings.EqualFold(boundHost, "localhost")
	if strings.EqualFold(host, "localhost") || (requestIP != nil && requestIP.IsLoopback()) {
		if loopback {
			return true
		}
		// A wildcard listener can still accept its actual local numeric address
		// below, but it does not acquire the localhost hostname alias.
		if requestIP == nil || ip == nil || !ip.IsUnspecified() {
			return false
		}
	}
	configuredHost, _, err := net.SplitHostPort(s.cfg.Server.Addr)
	if err == nil && net.ParseIP(configuredHost) == nil && configuredHost != "" && strings.EqualFold(host, configuredHost) {
		return true
	}
	if ip == nil || !ip.IsUnspecified() {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	localHost, localPort, err := net.SplitHostPort(local.String())
	return err == nil && localPort == port && strings.EqualFold(host, localHost)
}
