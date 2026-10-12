package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaskUser(t *testing.T) {
	cases := map[string]string{
		"0018067158": "00******58",
		"123456":     "12**56",
		"12345":      "12*45",
		"1234":       "****",
		"ab":         "**",
		"":           "",
	}
	for input, want := range cases {
		if got := maskUser(input); got != want {
			t.Errorf("maskUser(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseProfileSpec(t *testing.T) {
	cases := []struct {
		name      string
		dataTotal int64
		dataUnlim bool
		timeTotal int64
		timeUnlim bool
		price     float64
	}{
		{"1000-1G-8h", gib, false, 8 * 3600, false, 1000},
		{"1000-un-8h", 0, true, 8 * 3600, false, 1000},
		{"1000-1G-un", gib, false, 0, true, 1000},
		{"500-500M-30m", 500 * mib, false, 30 * 60, false, 500},
		{"2G-1d", 2 * gib, false, 86400, false, 0},
		{"-1G-08:30:00", gib, false, 30600, false, 0},
	}
	for _, tc := range cases {
		got := parseProfileSpec(tc.name)
		if got.DataTotal != tc.dataTotal || got.DataUnlimit != tc.dataUnlim {
			t.Errorf("%s: data = %d unlimited=%v, want %d unlimited=%v",
				tc.name, got.DataTotal, got.DataUnlimit, tc.dataTotal, tc.dataUnlim)
		}
		if got.TimeTotal != tc.timeTotal || got.TimeUnlimit != tc.timeUnlim {
			t.Errorf("%s: time = %d unlimited=%v, want %d unlimited=%v",
				tc.name, got.TimeTotal, got.TimeUnlimit, tc.timeTotal, tc.timeUnlim)
		}
		if got.Price != tc.price {
			t.Errorf("%s: price = %v, want %v", tc.name, got.Price, tc.price)
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := formatBytes(550 * mib); got != "550 MB" {
		t.Errorf("formatBytes = %q, want %q", got, "550 MB")
	}
	if got := formatBytes(1500 * mib); got != "1.5 GB" {
		t.Errorf("formatBytes = %q, want %q", got, "1.5 GB")
	}
	if got := formatDuration(8100); got != "2h 15m" {
		t.Errorf("formatDuration = %q, want %q", got, "2h 15m")
	}
	if got := formatDuration(45); got != "45s" {
		t.Errorf("formatDuration = %q, want %q", got, "45s")
	}
	if got := formatDuration(0); got != "0s" {
		t.Errorf("formatDuration = %q, want %q", got, "0s")
	}
}

// fakePortal is a stand-in for the RouterOS v7 User Manager web portal.
type fakePortal struct {
	server      *httptest.Server
	logoutCalls atomic.Int64
	loginCalls  atomic.Int64
	password    string
	username    string
	endTime     string
	state       string
	profile     string
}

func newFakePortal(t testing.TB, username, password, profile, state, endTime string) *fakePortal {
	t.Helper()
	portal := &fakePortal{password: password, username: username, profile: profile, state: state, endTime: endTime}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /um/api/login", func(w http.ResponseWriter, r *http.Request) {
		portal.loginCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		user, _ := body["username"].(string)
		pass, _ := body["password"].(string)
		if user != portal.username || pass != portal.password {
			writeTestJSON(w, http.StatusOK, map[string]any{"success": false, "error": "bad credentials"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "um_session", Value: "session-token", Path: "/"})
		writeTestJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	mux.HandleFunc("GET /um/api/getUser", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("um_session"); err != nil {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
			return
		}
		writeTestJSON(w, http.StatusOK, []any{map[string]any{
			"name":            portal.username,
			"actual-profile":  portal.profile,
			"download-used":   100 * mib,
			"upload-used":     50 * mib,
			"uptime":          5400,
			"active-sessions": 2,
			"state":           portal.state,
		}})
	})
	mux.HandleFunc("GET /um/api/getUserProfiles", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("um_session"); err != nil {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
			return
		}
		record := map[string]any{"user": portal.username, "profile": portal.profile, "state": portal.state}
		if portal.endTime != "" {
			record["end-time"] = portal.endTime
		}
		writeTestJSON(w, http.StatusOK, []any{record})
	})
	mux.HandleFunc("GET /um/api/getProfiles", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, []any{map[string]any{"name": portal.profile, "validity": "8h", "price": 1000}})
	})
	mux.HandleFunc("POST /um/api/logout", func(w http.ResponseWriter, r *http.Request) {
		portal.logoutCalls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"success": true})
	})

	portal.server = httptest.NewServer(mux)
	t.Cleanup(portal.server.Close)
	return portal
}

func writeTestJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func newTestServer(t testing.TB, portal *fakePortal, logoutDelay time.Duration) *httptest.Server {
	t.Helper()
	srv := &server{cfg: config{
		umHost:         portal.server.URL,
		listenAddr:     ":0",
		logoutDelay:    logoutDelay,
		requestTimeout: 5 * time.Second,
		allowedOrigin:  "*",
	}}
	front := httptest.NewServer(srv.routes())
	t.Cleanup(front.Close)
	return front
}

func TestStatusEndpointEndToEnd(t *testing.T) {
	portal := newFakePortal(t, "0018067158", "secret", "1000-1G-8h", "running active", "")
	front := newTestServer(t, portal, 50*time.Millisecond)

	resp, err := http.Get(front.URL + "/api/status?user=0018067158&pass=secret")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want %q", got, "*")
	}
	if strings.Contains(string(raw), "0018067158") || strings.Contains(string(raw), "secret") {
		t.Fatalf("response leaked credentials: %s", raw)
	}

	var payload struct {
		Success           bool     `json:"success"`
		MaskedUser        string   `json:"masked_user"`
		DataLeft          Quantity `json:"data_left"`
		TimeLeft          Quantity `json:"time_left"`
		RemainingValidity Quantity `json:"remaining_validity"`
		TotalValidity     Quantity `json:"total_validity"`
		ProfileState      string   `json:"profile_state"`
		ActiveSessions    int      `json:"active_sessions"`
		Profile           string   `json:"profile"`
		LimitsSource      string   `json:"limits_source"`
		Warnings          []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, raw)
	}

	if !payload.Success {
		t.Errorf("success = false")
	}
	if payload.MaskedUser != "00******58" {
		t.Errorf("masked_user = %q", payload.MaskedUser)
	}
	if want := int64(gib - 150*mib); payload.DataLeft.Raw != want {
		t.Errorf("data_left raw = %d, want %d", payload.DataLeft.Raw, want)
	}
	if payload.DataLeft.Human != "874 MB" {
		t.Errorf("data_left human = %q, want %q", payload.DataLeft.Human, "874 MB")
	}
	if want := int64(8*3600 - 5400); payload.TimeLeft.Raw != want {
		t.Errorf("time_left raw = %d, want %d", payload.TimeLeft.Raw, want)
	}
	if payload.TimeLeft.Human != "6h 30m" {
		t.Errorf("time_left human = %q, want %q", payload.TimeLeft.Human, "6h 30m")
	}
	if payload.TotalValidity.Raw != 8*3600 {
		t.Errorf("total_validity raw = %d, want %d", payload.TotalValidity.Raw, 8*3600)
	}
	if payload.ProfileState != "active" {
		t.Errorf("profile_state = %q, want %q", payload.ProfileState, "active")
	}
	if payload.ActiveSessions != 2 {
		t.Errorf("active_sessions = %d, want 2", payload.ActiveSessions)
	}
	if payload.Profile != "1000-1G-8h" {
		t.Errorf("profile = %q", payload.Profile)
	}
	if payload.LimitsSource != limitsSourceName {
		t.Errorf("limits_source = %q, want %q", payload.LimitsSource, limitsSourceName)
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", payload.Warnings)
	}

	// The delayed logout must fire once on its own.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if portal.logoutCalls.Load() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("delayed logout was not called, calls = %d", portal.logoutCalls.Load())
}

func TestStatusEndpointExpiredProfile(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	portal := newFakePortal(t, "0018067158", "", "1000-1G-8h", "used", past)
	front := newTestServer(t, portal, 0)

	resp, err := http.Get(front.URL + "/api/status?user=0018067158")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var payload struct {
		ProfileState      string   `json:"profile_state"`
		TimeLeft          Quantity `json:"time_left"`
		RemainingValidity Quantity `json:"remaining_validity"`
		ValidityEndsAt    string   `json:"validity_ends_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, body)
	}
	if payload.ProfileState != "expired" {
		t.Errorf("profile_state = %q, want expired", payload.ProfileState)
	}
	if payload.TimeLeft.Raw != 0 || payload.RemainingValidity.Raw != 0 {
		t.Errorf("expired profile must report zero remaining time, got %+v / %+v", payload.TimeLeft, payload.RemainingValidity)
	}
	if payload.ValidityEndsAt == "" {
		t.Errorf("validity_ends_at is empty")
	}
}

func TestStatusEndpointValidation(t *testing.T) {
	portal := newFakePortal(t, "0018067158", "secret", "1000-1G-8h", "running", "")
	front := newTestServer(t, portal, 0)

	cases := []struct {
		url        string
		wantStatus int
		wantCode   string
	}{
		{"/api/status", http.StatusBadRequest, "missing_user"},
		{"/api/status?user=0018067158&pass=wrong", http.StatusBadGateway, "login_failed"},
		{"/unknown", http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		resp, err := http.Get(front.URL + tc.url)
		if err != nil {
			t.Fatalf("request %s failed: %v", tc.url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.url, resp.StatusCode, tc.wantStatus)
		}
		var payload errorResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: invalid JSON: %v", tc.url, err)
		}
		if payload.Code != tc.wantCode || payload.Success {
			t.Errorf("%s: payload = %+v, want code %q with success=false", tc.url, payload, tc.wantCode)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	portal := newFakePortal(t, "user", "pass", "1000-1G-8h", "running", "")
	front := newTestServer(t, portal, 60*time.Second)

	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestConcurrentRequestsStayIsolated(t *testing.T) {
	portal := newFakePortal(t, "0018067158", "secret", "1000-1G-8h", "running active", "")
	front := newTestServer(t, portal, 10*time.Millisecond)

	const parallel = 8
	done := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			resp, err := http.Get(front.URL + "/api/status?user=0018067158&pass=secret")
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				done <- err
				return
			}
			if resp.StatusCode != http.StatusOK {
				done <- fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
				return
			}
			done <- nil
		}()
	}
	for i := 0; i < parallel; i++ {
		if err := <-done; err != nil {
			t.Fatalf("parallel request failed: %v", err)
		}
	}
	if got := portal.loginCalls.Load(); got != parallel {
		t.Errorf("portal login calls = %d, want %d (one session per request)", got, parallel)
	}
}

func TestBuildStatusUnlimitedData(t *testing.T) {
	portalData := portalData{
		user: []any{map[string]any{
			"name":           "0018067158",
			"actual-profile": "1000-un-8h",
			"download-used":  5 * gib,
			"upload-used":    1 * gib,
			"uptime":         3600,
		}},
		userProfiles: []any{map[string]any{"user": "0018067158", "profile": "1000-un-8h", "state": "running active"}},
		profiles:     []any{map[string]any{"name": "1000-un-8h"}},
	}
	response := buildStatus("0018067158", portalData, time.Now())
	if !response.DataLeft.Unlimited || response.DataLeft.Human != "unlimited" {
		t.Errorf("data_left = %+v, want unlimited", response.DataLeft)
	}
	if response.TimeLeft.Raw != 8*3600-3600 {
		t.Errorf("time_left raw = %d, want %d", response.TimeLeft.Raw, 8*3600-3600)
	}
}

// BenchmarkStatusEndpoint measures the full handler flow against an in-process
// portal, which isolates the gateway overhead from external latency.
func BenchmarkStatusEndpoint(b *testing.B) {
	portal := newFakePortal(b, "0018067158", "secret", "1000-1G-8h", "running active", "")
	front := newTestServer(b, portal, time.Hour)
	client := &http.Client{Timeout: 5 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(front.URL + "/api/status?user=0018067158&pass=secret")
		if err != nil {
			b.Fatalf("request failed: %v", err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatalf("reading response failed: %v", err)
		}
		_ = resp.Body.Close()
	}
}
