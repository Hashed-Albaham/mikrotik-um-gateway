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
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Environment variables. Either UM_TARGET_URL or UM_HOST is mandatory;
// everything else has a documented default so the container can be started with
// plain defaults.
const (
	envUMTargetURL   = "UM_TARGET_URL"
	envUMHost        = "UM_HOST"
	envUMProto       = "UM_PROTO"
	envUMPort        = "UM_PORT"
	envUMSkipVerify  = "UM_INSECURE_SKIP_VERIFY"
	envListenAddr    = "LISTEN_ADDR"
	envPort          = "PORT"
	envLogoutDelay   = "LOGOUT_DELAY_SECONDS"
	envRequestTimout = "REQUEST_TIMEOUT_SECONDS"
	envAllowedOrigin = "ALLOWED_ORIGIN"
	envReleaseMemory = "RELEASE_MEMORY_ON_SESSION_END"

	defaultListenAddr      = ":8080"
	defaultLogoutDelaySecs = 60
	defaultRequestTimeout  = 6 * time.Second
	defaultAllowedOrigin   = "*"
	logoutCallTimeout      = 20 * time.Second
	maxRequestBodyBytes    = 1 << 10
)

// config holds the runtime configuration of the gateway.
type config struct {
	umHost         string
	listenAddr     string
	logoutDelay    time.Duration
	requestTimeout time.Duration
	allowedOrigin  string
	releaseMemory  bool
	skipTLSVerify  bool
}

// loadConfig reads the environment and validates it.
func loadConfig() (config, error) {
	cfg := config{
		listenAddr:     strings.TrimSpace(os.Getenv(envListenAddr)),
		logoutDelay:    defaultLogoutDelaySecs * time.Second,
		requestTimeout: defaultRequestTimeout,
		allowedOrigin:  defaultAllowedOrigin,
		releaseMemory:  true,
		skipTLSVerify:  true,
	}

	baseURL, err := resolveUMBaseURL()
	if err != nil {
		return config{}, err
	}
	cfg.umHost = baseURL

	if cfg.listenAddr == "" {
		cfg.listenAddr = defaultListenAddr
		if raw := strings.TrimSpace(os.Getenv(envPort)); raw != "" {
			port, err := strconv.Atoi(raw)
			if err != nil || port <= 0 || port > 65535 {
				return config{}, fmt.Errorf("%s must be a valid TCP port, got %q", envPort, raw)
			}
			cfg.listenAddr = ":" + raw
		}
	}
	if origin := strings.TrimSpace(os.Getenv(envAllowedOrigin)); origin != "" {
		cfg.allowedOrigin = origin
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
	if raw := strings.TrimSpace(os.Getenv(envUMSkipVerify)); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s must be a boolean, got %q", envUMSkipVerify, raw)
		}
		cfg.skipTLSVerify = enabled
	}
	return cfg, nil
}

// resolveUMBaseURL builds the portal base URL from either a full URL or from
// the protocol, host and port parts.
func resolveUMBaseURL() (string, error) {
	if raw := strings.TrimSpace(os.Getenv(envUMTargetURL)); raw != "" {
		return normalizeBaseURL(raw)
	}

	host := strings.TrimSpace(os.Getenv(envUMHost))
	if host == "" {
		return "", fmt.Errorf("%s or %s is required (for example %s=http://192.168.88.1)", envUMTargetURL, envUMHost, envUMHost)
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return normalizeBaseURL(host)
	}

	proto := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(os.Getenv(envUMProto)), "://"))
	if proto == "" {
		proto = "http"
	}
	if proto != "http" && proto != "https" {
		return "", fmt.Errorf("%s must be http or https, got %q", envUMProto, proto)
	}
	host = strings.Trim(host, "/")
	if port := strings.TrimSpace(os.Getenv(envUMPort)); port != "" {
		if _, err := strconv.Atoi(port); err != nil || port == "" {
			return "", fmt.Errorf("%s must be a valid TCP port, got %q", envUMPort, port)
		}
		if !strings.Contains(host, ":") {
			host += ":" + port
		}
	}
	return normalizeBaseURL(proto + "://" + host)
}

// normalizeBaseURL validates a base URL and removes a trailing slash.
func normalizeBaseURL(raw string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return "", fmt.Errorf("user manager base URL is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid user manager base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("user manager base URL must use http or https, got %q", base)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("user manager base URL has no host: %q", base)
	}
	return base, nil
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
// "/api/status" and "/api/user-status" are the same handler under two names so
// existing hotspot pages keep working.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/status", s.handleStatus)
	mux.HandleFunc("OPTIONS /api/status", s.handlePreflight)
	mux.HandleFunc("GET /api/user-status", s.handleStatus)
	mux.HandleFunc("POST /api/user-status", s.handleStatus)
	mux.HandleFunc("OPTIONS /api/user-status", s.handlePreflight)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	return s.withCORS(mux)
}

// withCORS adds the CORS headers required by browser based hotspot pages.
func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.cfg.allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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
		"service": "um-status-gateway",
		"endpoints": []string{
			"GET|POST /api/status?user=<id>&pass=<password>",
			"GET|POST /api/user-status?user=<id>&pass=<password>",
			"GET /healthz",
		},
	})
}

// handleStatus is the only work producing endpoint. It performs the whole
// on-demand flow for a single account and answers before the delayed logout
// fires.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := r.ParseForm(); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		writeError(w, http.StatusBadRequest, "invalid_request", "the request body could not be parsed")
		return
	}

	user := strings.TrimSpace(firstNonEmpty(r.Form.Get("user"), r.Form.Get("username")))
	password := firstNonEmpty(r.Form.Get("pass"), r.Form.Get("password"))

	if user == "" {
		writeError(w, http.StatusBadRequest, "missing_user", "parameter 'user' is required")
		return
	}
	if len(user) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_user", "parameter 'user' is too long")
		return
	}
	if len(password) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_password", "parameter 'pass' is too long")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.requestTimeout)
	defer cancel()

	client, err := NewClient(s.cfg.umHost, s.cfg.requestTimeout, user, s.cfg.skipTLSVerify)
	if err != nil {
		log.Printf("status user=%s failed: %v", maskUser(user), err)
		writeError(w, http.StatusInternalServerError, "configuration_error", err.Error())
		return
	}

	if err := client.Login(ctx, password); err != nil {
		if errors.Is(err, errInvalidCredentials) {
			log.Printf("status user=%s rejected: invalid credentials", maskUser(user))
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "the user manager rejected these credentials")
			return
		}
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

	response := buildStatus(user, payloads.user, payloads.profiles, time.Now())
	writeJSON(w, http.StatusOK, response)

	// The portal session is closed by a single one-shot timer, never by a loop.
	s.scheduleLogout(client)
	log.Printf("status user=%s state=%s data_left=%s time_left=%s",
		response.MaskedUsername, response.ProfileState, response.DataLeft, response.TimeLeft)
}

// portalData carries the two concurrent User Manager responses.
type portalData struct {
	user     UserData
	profiles []ProfileEntry
}

// fetchAll issues the two read calls concurrently and collects their results.
// The first error is reported once both calls have finished.
func fetchAll(ctx context.Context, client *Client) (portalData, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		data     portalData
	)

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

	wg.Add(2)
	go func() {
		defer wg.Done()
		user, err := client.FetchUser(ctx)
		recordErr(err)
		mu.Lock()
		data.user = user
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		profiles, err := client.FetchUserProfiles(ctx)
		recordErr(err)
		mu.Lock()
		data.profiles = profiles
		mu.Unlock()
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
