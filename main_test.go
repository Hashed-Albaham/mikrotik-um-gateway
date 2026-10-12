package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaskUsername(t *testing.T) {
	cases := map[string]string{
		"0018067158": "00******58",
		"123456":     "12**56",
		"1234":       "****",
		"":           "",
	}
	for input, want := range cases {
		if got := maskUsername(input); got != want {
			t.Errorf("maskUsername(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParsePlan(t *testing.T) {
	cases := []struct {
		name      string
		data      int64
		dataUnlim bool
		time      int64
		timeUnlim bool
	}{
		{name: "1000-1G-8h", data: 1 << 30, time: 8 * 3600},
		{name: "1000-un-8h", dataUnlim: true, time: 8 * 3600},
		{name: "1000-1G-un", data: 1 << 30, timeUnlim: true},
		{name: "500-500M-30m", data: 500 << 20, time: 30 * 60},
		{name: "2G-1d", data: 2 << 30, time: 86400},
		{name: "1000-1.5G-08:30:00", data: 1610612736, time: 30600},
		{name: "", data: 0, time: 0},
	}
	for _, tc := range cases {
		data, dataUnlim, span, timeUnlim := parsePlan(tc.name)
		if data != tc.data || dataUnlim != tc.dataUnlim || span != tc.time || timeUnlim != tc.timeUnlim {
			t.Errorf("parsePlan(%q) = (%d,%v,%d,%v), want (%d,%v,%d,%v)",
				tc.name, data, dataUnlim, span, timeUnlim, tc.data, tc.dataUnlim, tc.time, tc.timeUnlim)
		}
	}
}

func TestParseValues(t *testing.T) {
	if got := parseSize("1.5G"); got != 1610612736 {
		t.Errorf("parseSize(1.5G) = %d", got)
	}
	if got := parseSize("123456"); got != 123456 {
		t.Errorf("parseSize(123456) = %d", got)
	}
	if got := parseSpan("2h30m"); got != 9000 {
		t.Errorf("parseSpan(2h30m) = %d", got)
	}
	if got := parseSpan("08:30:00"); got != 30600 {
		t.Errorf("parseSpan(08:30:00) = %d", got)
	}
	if _, ok := parseValidity("6h 30m"); !ok {
		t.Errorf("parseValidity(6h 30m) should succeed")
	}
	if !isExpired(json.RawMessage("3")) {
		t.Errorf("state 3 must be expired")
	}
	if isExpired(json.RawMessage("1")) {
		t.Errorf("state 1 must be active")
	}
}

// بوابة وهمية تحاكي اليوزر منجر كما هو في RouterOS v7.
func newFakePortal(t *testing.T, state json.RawMessage, expAfter string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var logouts atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/um/api/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("username") != "0018067158" || r.PostForm.Get("password") != "secret" {
			writeTestJSON(w, http.StatusOK, map[string]any{"success": false})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "um_session", Value: "token", Path: "/"})
		writeTestJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	mux.HandleFunc("/um/api/getUser", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("um_session"); err != nil {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{
				"username":    "0018067158",
				"download":    "100M",
				"upload":      "50M",
				"uptime":      "1h30m",
				"activeSess":  2,
				"sharedUsers": 1,
			},
		})
	})
	mux.HandleFunc("/um/api/getUserProfiles", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("um_session"); err != nil {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "no session"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data": map[string]any{"profiles": []any{map[string]any{
				"name": "1000-1G-8h", "profileId": 5, "state": state, "expAfter": expAfter,
			}}},
		})
	})
	mux.HandleFunc("/um/api/logout", func(w http.ResponseWriter, r *http.Request) {
		logouts.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"success": true})
	})

	portal := httptest.NewServer(mux)
	t.Cleanup(portal.Close)
	return portal, &logouts
}

func writeTestJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func startGateway(t *testing.T, portalURL string, logoutDelay time.Duration) *httptest.Server {
	t.Helper()
	cfg = config{
		portalURL:   portalURL,
		listenAddr:  ":0",
		logoutDelay: logoutDelay,
		timeout:     5 * time.Second,
	}
	front := httptest.NewServer(http.HandlerFunc(handleStatus))
	t.Cleanup(front.Close)
	return front
}

func TestStatusEndToEnd(t *testing.T) {
	portal, logouts := newFakePortal(t, json.RawMessage("1"), "6h 30m")
	front := startGateway(t, portal.URL, 50*time.Millisecond)

	response, err := http.Get(front.URL + "/api/user-status?user=0018067158&pass=secret")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, "0018067158") || strings.Contains(text, "secret") {
		t.Fatalf("الاستجابة تحتوي بيانات حساسة: %s", text)
	}

	var payload statusResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !payload.Success || payload.MaskedUsername != "00******58" {
		t.Errorf("payload = %+v", payload)
	}
	if payload.DataLeft != "874 MB" || payload.TotalData != "1 GB" {
		t.Errorf("data = %q / %q, want 874 MB / 1 GB", payload.DataLeft, payload.TotalData)
	}
	if payload.TimeLeft != "6h 30m" || payload.TotalTime != "8h" {
		t.Errorf("time = %q / %q", payload.TimeLeft, payload.TotalTime)
	}
	if payload.RemainingValidity != "6h 30m" {
		t.Errorf("remaining_validity = %q", payload.RemainingValidity)
	}
	if payload.ProfileState != "نشط" || payload.ProfileName != "1000-1G-8h" {
		t.Errorf("state/name = %q / %q", payload.ProfileState, payload.ProfileName)
	}
	if payload.ActiveSessions != 2 || payload.SharedUsers != 1 {
		t.Errorf("sessions = %d / %d", payload.ActiveSessions, payload.SharedUsers)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if logouts.Load() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("لم يتم إغلاق الجلسة بعد المدة المحددة")
}

func TestExpiredProfileAndErrors(t *testing.T) {
	portal, _ := newFakePortal(t, json.RawMessage("3"), "0")
	front := startGateway(t, portal.URL, 0)

	response, err := http.Get(front.URL + "/api/status?user=0018067158&pass=secret")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var payload statusResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload.ProfileState != "منتهي" || payload.TimeLeft != "0" || payload.RemainingValidity != "0" {
		t.Errorf("البروفايل المنتهي لم يُقرأ صحيحاً: %+v", payload)
	}

	// بدون رقم كرت
	resp, err := http.Get(front.URL + "/api/status")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing user status = %d, want 400", resp.StatusCode)
	}

	// كلمة مرور خاطئة
	resp, err = http.Get(front.URL + "/api/status?user=0018067158&pass=wrong")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad credentials status = %d, want 401", resp.StatusCode)
	}

	// إرسال كلمة المرور في جسم الطلب بدل الرابط
	resp, err = http.PostForm(front.URL+"/api/status", url.Values{"user": {"0018067158"}, "pass": {"secret"}})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST status = %d, want 200", resp.StatusCode)
	}
}
