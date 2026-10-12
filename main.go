// Command um-status-gateway exposes a small, on-demand HTTP API in front of the
// MikroTik RouterOS v7 User Manager web portal (/um/).
//
// The gateway is deliberately stateless and event driven: it opens a portal
// session only when an HTTP request arrives, reads the quota, validity and
// session data for one account, computes the remaining balances, masks the
// account identifier, schedules a delayed logout, and answers with JSON.
// It never runs a polling loop, a cache warmer or a shared session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Environment variables. Only UM_HOST is mandatory; everything else has a
// documented default so the container can be started with plain defaults.
const (
	envUMHost        = "UM_HOST"
	envListenAddr    = "LISTEN_ADDR"
	envLogoutDelay   = "LOGOUT_DELAY_SECONDS"
	envRequestTimout = "REQUEST_TIMEOUT_SECONDS"
	envAllowedOrigin = "ALLOWED_ORIGIN"
	envReleaseMemory = "RELEASE_MEMORY_ON_SESSION_END"

	defaultListenAddr      = ":8080"
	defaultLogoutDelaySecs = 60
	defaultRequestTimeout  = 15 * time.Second
	defaultAllowedOrigin   = "*"
	logoutCallTimeout      = 20 * time.Second
)

// config holds the runtime configuration of the gateway.
type config struct {
	umHost         string
	listenAddr     string
	logoutDelay    time.Duration
	requestTimeout time.Duration
	allowedOrigin  string
	releaseMemory  bool
}

// loadConfig reads the environment and validates it.
func loadConfig() (config, error) {
	cfg := config{
		umHost:         strings.TrimSpace(os.Getenv(envUMHost)),
		listenAddr:     strings.TrimSpace(os.Getenv(envListenAddr)),
		logoutDelay:    defaultLogoutDelaySecs * time.Second,
		requestTimeout: defaultRequestTimeout,
		allowedOrigin:  defaultAllowedOrigin,
		releaseMemory:  true,
	}
	if cfg.listenAddr == "" {
		cfg.listenAddr = defaultListenAddr
	}
	if origin := strings.TrimSpace(os.Getenv(envAllowedOrigin)); origin != "" {
		cfg.allowedOrigin = origin
	}
	if cfg.umHost == "" {
		return config{}, fmt.Errorf("%s is required (for example http://192.168.88.1)", envUMHost)
	}
	if !strings.HasPrefix(cfg.umHost, "http://") && !strings.HasPrefix(cfg.umHost, "https://") {
		cfg.umHost = "http://" + cfg.umHost
	}
	if raw := strings.TrimSpace(os.Getenv(envLogoutDelay)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			return config{}, fmt.Errorf("%s must be a non-negative integer, got %q", envLogoutDelay, raw)
		}
		cfg.logoutDelay = time.Duration(seconds) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv(envRequestTimout)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			return config{}, fmt.Errorf("%s must be a positive integer, got %q", envRequestTimout, raw)
		}
		cfg.requestTimeout = time.Duration(seconds) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv(envReleaseMemory)); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s must be a boolean, got %q", envReleaseMemory, raw)
		}
		cfg.releaseMemory = enabled
	}
	return cfg, nil
}

// Quantity, StatusResponse and ErrorResponse form the public JSON contract.

// statusResponse is the unified payload returned for a successful lookup.
type statusResponse struct {
	Success           bool     `json:"success"`
	MaskedUser        string   `json:"masked_user"`
	DataLeft          Quantity `json:"data_left"`
	TimeLeft          Quantity `json:"time_left"`
	RemainingValidity Quantity `json:"remaining_validity"`
	TotalValidity     Quantity `json:"total_validity"`
	ProfileState      string   `json:"profile_state"`
	ActiveSessions    int      `json:"active_sessions"`

	Profile        string    `json:"profile,omitempty"`
	DataUsed       *Quantity `json:"data_used,omitempty"`
	TimeUsed       *Quantity `json:"time_used,omitempty"`
	ValidityEndsAt string    `json:"validity_ends_at,omitempty"`
	LimitsSource   string    `json:"limits_source,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
	GeneratedAt    string    `json:"generated_at"`
}

// errorResponse is returned for every failure, with a stable machine readable
// code so callers can branch without parsing prose.
type errorResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Error   string `json:"error"`
}

// server wires the configuration into an http.Handler.
type server struct {
	cfg config
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	srv := &server{cfg: cfg}

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.requestTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("um-status-gateway listening on %s (user manager: %s, logout delay: %s)",
			cfg.listenAddr, cfg.umHost, cfg.logoutDelay)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case sig := <-shutdown:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}

// routes builds the HTTP routing table wrapped in the CORS middleware.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("OPTIONS /api/status", s.handlePreflight)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	return s.withCORS(mux)
}

// withCORS adds the CORS headers required by browser based dashboards.
func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.cfg.allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.Header().Set("Vary", "Origin")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handlePreflight(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "logout_delay_seconds": int(s.cfg.logoutDelay.Seconds())})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not_found", "unknown path")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "um-status-gateway",
		"endpoints": []string{"GET /api/status?user=<id>&pass=<password>", "GET /healthz"},
	})
}

// handleStatus is the only work producing endpoint. It performs the whole
// on-demand flow for a single account and answers before the delayed logout
// fires.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	user := strings.TrimSpace(query.Get("user"))
	password := query.Get("pass")

	if user == "" {
		writeError(w, http.StatusBadRequest, "missing_user", "query parameter 'user' is required")
		return
	}
	if len(user) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_user", "query parameter 'user' is too long")
		return
	}
	if len(password) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_password", "query parameter 'pass' is too long")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.requestTimeout)
	defer cancel()

	client, err := NewClient(s.cfg.umHost, s.cfg.requestTimeout, user)
	if err != nil {
		log.Printf("status user=%s failed: %v", maskUser(user), err)
		writeError(w, http.StatusInternalServerError, "configuration_error", err.Error())
		return
	}

	if err := client.Login(ctx, password); err != nil {
		log.Printf("status user=%s: %v", maskUser(user), err)
		writeError(w, http.StatusBadGateway, "login_failed", "could not open a user manager session")
		return
	}

	payloads, err := fetchAll(ctx, client)
	if err != nil {
		log.Printf("status user=%s fetch failed: %v", maskUser(user), err)
		s.scheduleLogout(client)
		writeError(w, http.StatusBadGateway, "fetch_failed", "could not read data from the user manager")
		return
	}
	// The reads are done: keep the session cookie but release the sockets so a
	// pending session does not hold buffers for the whole logout delay.
	client.CloseIdleConnections()

	response := buildStatus(user, payloads, time.Now())
	writeJSON(w, http.StatusOK, response)

	// The portal session is closed by a single one-shot timer, never by a loop.
	s.scheduleLogout(client)
	log.Printf("status user=%s state=%s data_left=%s time_left=%s",
		response.MaskedUser, response.ProfileState, response.DataLeft.Human, response.TimeLeft.Human)
}

// fetchAll issues the three read calls concurrently and collects their results.
// The first error is reported once every call has finished.
func fetchAll(ctx context.Context, client *Client) (portalData, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		data     portalData
	)

	assign := func(target *any) func(any) {
		return func(value any) {
			mu.Lock()
			defer mu.Unlock()
			*target = value
		}
	}
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	// A semaphore keeps the concurrency at exactly three goroutines and bounds
	// the peak memory of a single request.
	sem := make(chan struct{}, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		value, err := client.FetchUser(ctx)
		recordErr(err)
		assign(&data.user)(value)
	}()
	go func() {
		defer wg.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		value, err := client.FetchUserProfiles(ctx)
		recordErr(err)
		assign(&data.userProfiles)(value)
	}()
	go func() {
		defer wg.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		value, err := client.FetchProfiles(ctx)
		recordErr(err)
		assign(&data.profiles)(value)
	}()
	wg.Wait()

	if firstErr != nil {
		return portalData{}, firstErr
	}
	return data, nil
}

// scheduleLogout invalidates the portal session after the configured delay.
// This one-shot timer is part of the required workflow; it is not a polling
// loop and it does not keep any other state alive.
func (s *server) scheduleLogout(client *Client) {
	if s.cfg.logoutDelay <= 0 {
		return
	}
	time.AfterFunc(s.cfg.logoutDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), logoutCallTimeout)
		defer cancel()
		if err := client.Logout(ctx); err != nil {
			log.Printf("delayed logout for %s failed: %v", maskUser(client.user), err)
		} else {
			log.Printf("delayed logout completed for %s", maskUser(client.user))
		}
		// The session is over: drop its sockets and return the request's memory
		// to the operating system. This is a one-shot step at the end of the
		// request lifecycle, not a polling loop.
		client.CloseIdleConnections()
		if s.cfg.releaseMemory {
			debug.FreeOSMemory()
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"success":false,"code":"encode_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError sends the standard error envelope. Messages never contain the
// account identifier or the password.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Success: false, Code: code, Error: message})
}
