package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		{"1000-1.5G-1w2d", 1610612736, false, 777600, false, 1000},
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

func TestParseValidity(t *testing.T) {
	now := time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC)
	cases := []struct {
		value     string
		seconds   int64
		unlimited bool
		ok        bool
	}{
		{"6h 30m", 23400, false, true},
		{"2d 5h", 190800, false, true},
		{"08:30:00", 30600, false, true},
		{"unlimited", 0, true, true},
		{"un", 0, true, true},
		{"0", 0, false, true},
		{"-", 0, false, true},
		{"2026-10-13 04:00:00", 6 * 3600, false, true},
		{"2026-10-11 22:00:00", 0, false, true},
		{"", 0, false, false},
		{"...", 0, false, false},
	}
	for _, tc := range cases {
		seconds, unlimited, ok := parseValidity(tc.value, now)
		if ok != tc.ok || unlimited != tc.unlimited || seconds != tc.seconds {
			t.Errorf("parseValidity(%q) = (%d, %v, %v), want (%d, %v, %v)",
				tc.value, seconds, unlimited, ok, tc.seconds, tc.unlimited, tc.ok)
		}
	}
}

func TestResolveUMBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{
			name: "full target url wins",
			env:  map[string]string{"UM_TARGET_URL": "https://router.example:12008/", "UM_HOST": "10.0.0.1"},
			want: "https://router.example:12008",
		},
		{
			name: "host with scheme",
			env:  map[string]string{"UM_HOST": "http://192.168.88.1"},
			want: "http://192.168.88.1",
		},
		{
			name: "host with protocol and port",
			env:  map[string]string{"UM_HOST": "192.168.216.1", "UM_PORT": "12008", "UM_PROTO": "http"},
			want: "http://192.168.216.1:12008",
		},
		{
			name: "host only defaults to http",
			env:  map[string]string{"UM_HOST": "192.168.88.1"},
			want: "http://192.168.88.1",
		},
		{
			name: "explicit port in host is kept",
			env:  map[string]string{"UM_HOST": "router:12008", "UM_PORT": "8080"},
			want: "http://router:12008",
		},
		{
			name:    "unsupported protocol",
			env:     map[string]string{"UM_HOST": "router", "UM_PROTO": "ftp"},
			wantErr: true,
		},
		{
			name:    "nothing configured",
			env:     map[string]string{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envUMTargetURL, "")
			t.Setenv(envUMHost, "")
			t.Setenv(envUMProto, "")
			t.Setenv(envUMPort, "")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			got, err := resolveUMBaseURL()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveUMBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// portalOptions describes the account the fake portal serves.
type portalOptions struct {
	username string
	password string
	profile  string
	state    any    // numeric state used by the documented portal shape
	expAfter string // formatted remaining validity
	legacy   bool   // answer with top level keys and bare arrays instead
}

// fakePortal is a stand-in for the RouterOS v7 User Manager web portal.
type fakePortal struct {
	server      *httptest.Server
	logoutCalls atomic.Int64
	loginCalls  atomic.Int64
	lastLogin   atomic.Value // url.Values
	options     portalOptions
}

func newFakePortal(t testing.TB, options portalOptions) *fakePortal {
	t.Helper()
	portal := &fakePortal{options: options}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /um/api/login", func(w http.ResponseWriter, r *http.Request) {
		portal.loginCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"success": false})
			return
		}
		portal.lastLogin.Store(r.PostForm)
		if r.PostForm.Get("username") != options.username || r.PostForm.Get("password") != options.password {
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
		if options.legacy {
			writeTestJSON(w, http.StatusOK, []any{map[string]any{
				"name":            options.username,
				"download-used":   100 * mib,
				"upload-used":     50 * mib,
				"uptime":          5400,
				"active-sessions": 2,
				"state":           "running active",
			}})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{
				"username":    options.username,
				"download":    "100M",
				"upload":      "50M",
				"uptime":      "1h30m",
				"activeSess":  2,
				"sharedUsers": 1,
			},
		})
	})
	mux.HandleFunc("GET /um/api/getUserProfiles", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("um_session"); err != nil {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
			return
		}
		data := map[string]any{
			"id":        1,
			"name":      options.profile,
			"profileId": 5,
			"state":     options.state,
		}
		if options.expAfter != "" {
			data["expAfter"] = options.expAfter
		}
		if options.legacy {
			writeTestJSON(w, http.StatusOK, []any{map[string]any{
				"name":    options.username,
				"profile": options.profile,
				"state":   "running active",
			}})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data":    map[string]any{"profiles": []any{data}},
		})
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
		releaseMemory:  false,
		skipTLSVerify:  true,
	}}
	front := httptest.NewServer(srv.routes())
	t.Cleanup(front.Close)
	return front
}

// statusPayload mirrors the public contract for assertions.
type statusPayload struct {
	Success           bool   `json:"success"`
	MaskedUsername    string `json:"masked_username"`
	MaskedUser        string `json:"masked_user"`
	ProfileName       string `json:"profile_name"`
	ProfileState      string `json:"profile_state"`
	DataLeft          string `json:"data_left"`
	TotalData         string `json:"total_data"`
	TimeLeft          string `json:"time_left"`
	TotalTime         string `json:"total_time"`
	TotalValidity     string `json:"total_validity"`
	RemainingValidity string `json:"remaining_validity"`
	UsedDownload      string `json:"used_download"`
	UsedUpload        string `json:"used_upload"`
	UsedUptime        string `json:"used_uptime"`
	Sessions          struct {
		ActiveSessions int `json:"active_sessions"`
		SharedUsers    int `json:"shared_users"`
	} `json:"sessions"`
	ActiveSessions int `json:"active_sessions"`
	SharedUsers    int `json:"shared_users"`
	Raw            struct {
		DownloadBytes            int64 `json:"download_bytes"`
		UploadBytes              int64 `json:"upload_bytes"`
		UsedDataBytes            int64 `json:"used_data_bytes"`
		UptimeSeconds            int64 `json:"uptime_seconds"`
		DataLeftBytes            int64 `json:"data_left_bytes"`
		TotalDataBytes           int64 `json:"total_data_bytes"`
		TimeLeftSeconds          int64 `json:"time_left_seconds"`
		TotalTimeSeconds         int64 `json:"total_time_seconds"`
		RemainingValiditySeconds int64 `json:"remaining_validity_seconds"`
		DataUnlimited            bool  `json:"data_unlimited"`
		TimeUnlimited            bool  `json:"time_unlimited"`
	} `json:"raw"`
	LimitsSource string   `json:"limits_source"`
	Warnings     []string `json:"warnings"`
}

func fetchStatus(t *testing.T, url string) (statusPayload, string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var payload statusPayload
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("invalid JSON: %v (%s)", err, body)
		}
	}
	return payload, string(body), resp.StatusCode
}

func TestStatusEndpointEndToEnd(t *testing.T) {
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "secret",
		profile:  "1000-1G-8h",
		state:    1,
		expAfter: "6h 30m",
	})
	front := newTestServer(t, portal, 50*time.Millisecond)

	payload, raw, status := fetchStatus(t, front.URL+"/api/status?user=0018067158&pass=secret")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if strings.Contains(raw, "0018067158") || strings.Contains(raw, "secret") {
		t.Fatalf("response leaked credentials: %s", raw)
	}
	if !payload.Success {
		t.Errorf("success = false")
	}
	if payload.MaskedUsername != "00******58" || payload.MaskedUser != payload.MaskedUsername {
		t.Errorf("masked identifiers = %q / %q", payload.MaskedUsername, payload.MaskedUser)
	}
	if payload.ProfileName != "1000-1G-8h" {
		t.Errorf("profile_name = %q", payload.ProfileName)
	}
	if payload.ProfileState != "active" {
		t.Errorf("profile_state = %q, want active", payload.ProfileState)
	}
	if payload.DataLeft != "874 MB" || payload.TotalData != "1 GB" {
		t.Errorf("data_left/total_data = %q / %q, want 874 MB / 1 GB", payload.DataLeft, payload.TotalData)
	}
	if payload.TimeLeft != "6h 30m" || payload.TotalTime != "8h" || payload.TotalValidity != "8h" {
		t.Errorf("time_left/total_time/total_validity = %q / %q / %q", payload.TimeLeft, payload.TotalTime, payload.TotalValidity)
	}
	if payload.RemainingValidity != "6h 30m" {
		t.Errorf("remaining_validity = %q, want 6h 30m", payload.RemainingValidity)
	}
	if payload.UsedDownload != "100M" || payload.UsedUpload != "50M" || payload.UsedUptime != "1h30m" {
		t.Errorf("used counters = %q / %q / %q", payload.UsedDownload, payload.UsedUpload, payload.UsedUptime)
	}
	if payload.Sessions.ActiveSessions != 2 || payload.Sessions.SharedUsers != 1 || payload.ActiveSessions != 2 || payload.SharedUsers != 1 {
		t.Errorf("sessions = %+v (top level %d/%d)", payload.Sessions, payload.ActiveSessions, payload.SharedUsers)
	}
	if payload.Raw.DownloadBytes != 100*mib || payload.Raw.UploadBytes != 50*mib || payload.Raw.UsedDataBytes != 150*mib {
		t.Errorf("raw counters = %+v", payload.Raw)
	}
	if payload.Raw.DataLeftBytes != gib-150*mib || payload.Raw.TotalDataBytes != gib {
		t.Errorf("raw data balance = %d / %d", payload.Raw.DataLeftBytes, payload.Raw.TotalDataBytes)
	}
	if payload.Raw.UptimeSeconds != 5400 || payload.Raw.TimeLeftSeconds != 23400 || payload.Raw.TotalTimeSeconds != 28800 {
		t.Errorf("raw time values = %+v", payload.Raw)
	}
	if payload.Raw.RemainingValiditySeconds != 23400 {
		t.Errorf("raw remaining validity = %d, want 23400", payload.Raw.RemainingValiditySeconds)
	}
	if payload.LimitsSource != limitsSourceName {
		t.Errorf("limits_source = %q, want %q", payload.LimitsSource, limitsSourceName)
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", payload.Warnings)
	}

	// The login must travel as a form body, the way the portal expects it.
	form, ok := portal.lastLogin.Load().(url.Values)
	if !ok {
		t.Fatalf("the portal did not receive a form encoded login")
	}
	if form.Get("username") != "0018067158" || form.Get("password") != "secret" {
		t.Errorf("login form = %v", form)
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

func TestStatusEndpointViaAliasPathAndPost(t *testing.T) {
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "secret",
		profile:  "1000-un-8h",
		state:    1,
		expAfter: "2h",
	})
	front := newTestServer(t, portal, 0)

	// Alias path with a GET request.
	payload, raw, status := fetchStatus(t, front.URL+"/api/user-status?user=0018067158&pass=secret")
	if status != http.StatusOK {
		t.Fatalf("GET alias status = %d, body = %s", status, raw)
	}
	if payload.DataLeft != "unlimited" || !payload.Raw.DataUnlimited {
		t.Errorf("unlimited plan reported as %q (raw %+v)", payload.DataLeft, payload.Raw)
	}

	// POST with a form body keeps the credentials out of the URL.
	form := url.Values{"user": {"0018067158"}, "pass": {"secret"}}
	resp, err := http.PostForm(front.URL+"/api/user-status", form)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("CORS header = %q, want %q", resp.Header.Get("Access-Control-Allow-Origin"), "*")
	}
}

func TestStatusEndpointExpiredProfile(t *testing.T) {
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "",
		profile:  "1000-1G-8h",
		state:    stateExpired,
		expAfter: "0",
	})
	front := newTestServer(t, portal, 0)

	payload, _, status := fetchStatus(t, front.URL+"/api/status?user=0018067158")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if payload.ProfileState != "expired" {
		t.Errorf("profile_state = %q, want expired", payload.ProfileState)
	}
	if payload.TimeLeft != "0s" || payload.RemainingValidity != "0s" {
		t.Errorf("expired profile must report zero remaining time, got %q / %q", payload.TimeLeft, payload.RemainingValidity)
	}
	if payload.Raw.TimeLeftSeconds != 0 || payload.Raw.RemainingValiditySeconds != 0 {
		t.Errorf("raw remaining time = %+v", payload.Raw)
	}
}

func TestStatusEndpointLegacyShape(t *testing.T) {
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "",
		profile:  "1000-1G-8h",
		legacy:   true,
	})
	front := newTestServer(t, portal, 0)

	payload, raw, status := fetchStatus(t, front.URL+"/api/status?user=0018067158")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if payload.ProfileName != "1000-1G-8h" {
		t.Errorf("profile_name = %q", payload.ProfileName)
	}
	if payload.Raw.DownloadBytes != 100*mib || payload.Raw.UploadBytes != 50*mib {
		t.Errorf("raw counters were not recovered from the legacy shape: %+v", payload.Raw)
	}
	if payload.DataLeft != "874 MB" {
		t.Errorf("data_left = %q, want 874 MB", payload.DataLeft)
	}
	if payload.TimeLeft != "6h 30m" {
		t.Errorf("time_left = %q, want 6h 30m", payload.TimeLeft)
	}
	if payload.ProfileState != "active" {
		t.Errorf("profile_state = %q, want active", payload.ProfileState)
	}
}

func TestStatusEndpointValidation(t *testing.T) {
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "secret",
		profile:  "1000-1G-8h",
		state:    1,
	})
	front := newTestServer(t, portal, 0)

	cases := []struct {
		url        string
		wantStatus int
		wantCode   string
	}{
		{"/api/status", http.StatusBadRequest, "missing_user"},
		{"/api/status?user=0018067158&pass=wrong", http.StatusUnauthorized, "invalid_credentials"},
		{"/unknown", http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		payload, body, status := fetchStatus(t, front.URL+tc.url)
		if status != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.url, status, tc.wantStatus)
		}
		var envelope errorResponse
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("%s: invalid JSON: %v (%s)", tc.url, err, body)
		}
		if envelope.Code != tc.wantCode || envelope.Success || payload.Success {
			t.Errorf("%s: envelope = %+v, want code %q with success=false", tc.url, envelope, tc.wantCode)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	portal := newFakePortal(t, portalOptions{username: "user", password: "pass", profile: "1000-1G-8h", state: 1})
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
	portal := newFakePortal(t, portalOptions{
		username: "0018067158",
		password: "secret",
		profile:  "1000-1G-8h",
		state:    1,
	})
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

// BenchmarkStatusEndpoint measures the full handler flow against an in-process
// portal, which isolates the gateway overhead from external latency.
func BenchmarkStatusEndpoint(b *testing.B) {
	portal := newFakePortal(b, portalOptions{
		username: "0018067158",
		password: "secret",
		profile:  "1000-1G-8h",
		state:    1,
		expAfter: "6h 30m",
	})
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
