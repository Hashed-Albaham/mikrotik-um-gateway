package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// User Manager web portal endpoints. These are the JSON endpoints served by the
// User Manager web interface (/um/) on RouterOS v7.
//
//	POST /um/api/login            form encoded: username, password
//	GET  /um/api/getUser          session scoped snapshot of the account
//	GET  /um/api/getUserProfiles  the profiles assigned to the account
//	POST /um/api/logout           ends the session
const (
	apiLogin           = "/um/api/login"
	apiGetUser         = "/um/api/getUser"
	apiGetUserProfiles = "/um/api/getUserProfiles"
	apiLogout          = "/um/api/logout"
)

const (
	maxResponseBytes = 2 << 20 // 2 MiB guard against oversized portal responses
	maxJSONDepth     = 10
)

// errInvalidCredentials reports a rejected login, so the caller can answer with
// an authentication error instead of a gateway error.
var errInvalidCredentials = errors.New("invalid credentials")

// Client is a single-use User Manager client. Every HTTP request handled by the
// gateway builds its own Client, which means its cookie jar is isolated and no
// session is ever shared or reused between callers.
type Client struct {
	baseURL string
	user    string
	http    *http.Client
}

// NewClient prepares a client with a private in-memory cookie jar. The
// skipVerify flag mirrors the RouterOS habit of serving the portal over https
// with a self signed certificate.
func NewClient(baseURL string, timeout time.Duration, user string, skipVerify bool) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("user manager base URL is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid user manager base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("user manager base URL must use http or https")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}
	return &Client{
		baseURL: base,
		user:    user,
		http: &http.Client{
			Jar:     jar,
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     15 * time.Second,
				DisableCompression:  true,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: skipVerify}, //nolint:gosec // routers use self signed certificates
			},
		},
	}, nil
}

// do performs one portal call and returns the raw body of a successful answer.
func (c *Client) do(ctx context.Context, method, path string, form url.Values) ([]byte, error) {
	target := c.baseURL + path

	var body io.Reader
	contentType := ""
	if form != nil {
		encoded := form.Encode()
		body = strings.NewReader(encoded)
		contentType = "application/x-www-form-urlencoded"
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.8")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "um-status-gateway/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s returned HTTP %d: %w", path, resp.StatusCode, errInvalidCredentials)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, snippet(raw))
	}
	return raw, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// Login authenticates against the User Manager portal with a form encoded body
// and stores the resulting session cookie in the client's private jar.
func (c *Client) Login(ctx context.Context, password string) error {
	form := url.Values{}
	form.Set("username", c.user)
	form.Set("password", password)

	raw, err := c.do(ctx, http.MethodPost, apiLogin, form)
	if err != nil {
		if errors.Is(err, errInvalidCredentials) {
			return errInvalidCredentials
		}
		return fmt.Errorf("login failed: %w", err)
	}

	var typed struct {
		Success *bool `json:"success"`
		Error   any   `json:"error"`
	}
	_ = json.Unmarshal(raw, &typed)
	if typed.Success != nil {
		if !*typed.Success {
			return errInvalidCredentials
		}
		return nil
	}

	// Some builds answer without a success flag; fall back to a generic look.
	var generic any
	if err := json.Unmarshal(raw, &generic); err == nil {
		if value, found := lookupFirst(generic, "success", "ok"); found {
			flag, ok := toBool(value)
			if !ok {
				return nil
			}
			if !flag {
				return errInvalidCredentials
			}
			return nil
		}
	}
	return nil
}

// UserData is one normalized snapshot of an account.
type UserData struct {
	Username       string
	DownloadText   string
	UploadText     string
	UptimeText     string
	DownloadBytes  int64
	UploadBytes    int64
	UptimeSeconds  int64
	HasDownload    bool
	HasUpload      bool
	HasUptime      bool
	ActiveSessions int
	HasSessions    bool
	SharedUsers    int
	HasSharedUsers bool
	Success        bool
}

// ProfileEntry is one row of GET /um/api/getUserProfiles data.profiles[].
type ProfileEntry struct {
	ID         int
	Name       string
	ProfileID  int
	State      any // numeric on RouterOS v7, textual on some builds
	ExpAfter   string
	HasExpiry  bool
	StateValue int64
}

// FetchUser reads the account snapshot of the logged in session. The portal
// scopes this call to the session cookie, so no account identifier is sent.
func (c *Client) FetchUser(ctx context.Context) (UserData, error) {
	raw, err := c.do(ctx, http.MethodGet, apiGetUser, nil)
	if err != nil {
		return UserData{}, err
	}

	var typed struct {
		Success *bool `json:"success"`
		Data    struct {
			Username       *string `json:"username"`
			Download       *string `json:"download"`
			Upload         *string `json:"upload"`
			Uptime         *string `json:"uptime"`
			ActiveSessions *int    `json:"activeSess"`
			SharedUsers    *int    `json:"sharedUsers"`
		} `json:"data"`
	}
	typedOK := json.Unmarshal(raw, &typed) == nil

	data := UserData{Success: true}
	if typedOK {
		if typed.Success != nil {
			data.Success = *typed.Success
		}
		if typed.Data.Username != nil {
			data.Username = strings.TrimSpace(*typed.Data.Username)
		}
		if typed.Data.Download != nil {
			data.DownloadText = strings.TrimSpace(*typed.Data.Download)
			data.DownloadBytes, data.HasDownload = parseCounter(data.DownloadText)
		}
		if typed.Data.Upload != nil {
			data.UploadText = strings.TrimSpace(*typed.Data.Upload)
			data.UploadBytes, data.HasUpload = parseCounter(data.UploadText)
		}
		if typed.Data.Uptime != nil {
			data.UptimeText = strings.TrimSpace(*typed.Data.Uptime)
			data.UptimeSeconds, data.HasUptime = parseCounter(data.UptimeText)
		}
		if typed.Data.ActiveSessions != nil {
			data.ActiveSessions, data.HasSessions = *typed.Data.ActiveSessions, true
		}
		if typed.Data.SharedUsers != nil {
			data.SharedUsers, data.HasSharedUsers = *typed.Data.SharedUsers, true
		}
	}

	// Older or unusual builds return a different shape; fill whatever is still
	// missing from a generic walk of the same payload.
	jsonOK := typedOK
	if !data.complete() {
		var generic any
		if err := json.Unmarshal(raw, &generic); err == nil {
			jsonOK = true
			fillUserData(&data, generic)
		}
	}
	if !jsonOK {
		return UserData{}, fmt.Errorf("decoding %s: the portal did not answer with JSON", apiGetUser)
	}
	return data, nil
}

// complete reports whether the snapshot carries every value the payload needs.
func (d UserData) complete() bool {
	return d.Username != "" && d.HasDownload && d.HasUpload && d.HasUptime && d.HasSessions && d.HasSharedUsers
}

// fetchUserProfilesResponse mirrors the shape of the profiles endpoint.
func (c *Client) FetchUserProfiles(ctx context.Context) ([]ProfileEntry, error) {
	raw, err := c.do(ctx, http.MethodGet, apiGetUserProfiles, nil)
	if err != nil {
		return nil, err
	}

	var typed struct {
		Success *bool `json:"success"`
		Data    struct {
			Profiles []struct {
				ID        *int            `json:"id"`
				Name      *string         `json:"name"`
				ProfileID *int            `json:"profileId"`
				State     json.RawMessage `json:"state"`
				ExpAfter  *string         `json:"expAfter"`
			} `json:"profiles"`
		} `json:"data"`
	}
	typedOK := json.Unmarshal(raw, &typed) == nil

	entries := make([]ProfileEntry, 0, len(typed.Data.Profiles))
	if typedOK {
		for _, row := range typed.Data.Profiles {
			entry := ProfileEntry{}
			if row.ID != nil {
				entry.ID = *row.ID
			}
			if row.Name != nil {
				entry.Name = strings.TrimSpace(*row.Name)
			}
			if row.ProfileID != nil {
				entry.ProfileID = *row.ProfileID
			}
			if len(row.State) > 0 {
				entry.State = decodeStateValue(row.State)
			}
			if row.ExpAfter != nil {
				entry.ExpAfter = strings.TrimSpace(*row.ExpAfter)
				entry.HasExpiry = entry.ExpAfter != ""
			}
			if number, ok := toInt64(entry.State); ok {
				entry.StateValue = number
			}
			entries = append(entries, entry)
		}
	}
	if len(entries) > 0 {
		return entries, nil
	}

	// Fallback for builds that return a bare array or another wrapper.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		if !typedOK {
			return nil, fmt.Errorf("decoding %s: the portal did not answer with JSON", apiGetUserProfiles)
		}
		return nil, nil
	}
	return profilesFromGeneric(generic), nil
}

// decodeStateValue keeps the state usable whether it arrived as a number or as
// a word such as "running active" or "used".
func decodeStateValue(raw json.RawMessage) any {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return nil
}

// Logout ends the portal session using the same cookie jar.
func (c *Client) Logout(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.user)
	if _, err := c.do(ctx, http.MethodPost, apiLogout, form); err != nil {
		if errors.Is(err, errInvalidCredentials) {
			return nil // the session is already gone, which is the desired outcome
		}
		return fmt.Errorf("logout failed: %w", err)
	}
	return nil
}

// CloseIdleConnections releases the sockets the client kept alive. It is called
// once the request lifecycle of a session is finished.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// parseCounter turns a portal counter into a number. Counters arrive either as
// a plain number or as a formatted string such as "1.2G" or "2h30m".
func parseCounter(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return value, true
	}
	if value, err := strconv.ParseFloat(text, 64); err == nil {
		return int64(value), true
	}
	if bytesValue, _, ok := parseDataSize(text); ok {
		return bytesValue, true
	}
	if seconds, _, ok := parseTimeSpan(text); ok {
		return seconds, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Tolerant JSON access helpers, used only to fill gaps when a build answers with
// a shape that differs from the documented one: keys written as "download-used"
// or "activeSessions", values nested one level deeper, and so on.
// ---------------------------------------------------------------------------

// normalizeKey lowercases a JSON key and removes separators for comparison.
func normalizeKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		switch r {
		case '-', '_', ' ', '.', '/':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lookupFirst returns the first value found for any of the given key aliases.
// Aliases are searched in the given order and, for each alias, the shallowest
// match wins, so the result does not depend on Go's map iteration order.
func lookupFirst(node any, aliases ...string) (any, bool) {
	for _, alias := range aliases {
		if value, ok := findKey(node, normalizeKey(alias)); ok {
			return value, true
		}
	}
	return nil, false
}

// findKey walks a decoded JSON value breadth first and returns the first value
// whose key matches the normalized wanted key.
func findKey(node any, wanted string) (any, bool) {
	type item struct {
		value any
		depth int
	}
	queue := []item{{value: node}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth > maxJSONDepth {
			continue
		}
		switch typed := current.value.(type) {
		case map[string]any:
			for key, value := range typed {
				if normalizeKey(key) == wanted {
					return value, true
				}
			}
			for _, value := range typed {
				queue = append(queue, item{value: value, depth: current.depth + 1})
			}
		case []any:
			for _, value := range typed {
				queue = append(queue, item{value: value, depth: current.depth + 1})
			}
		}
	}
	return nil, false
}

// collect walks a decoded JSON value and gathers every value whose key matches
// one of the aliases.
func collect(node any, aliases ...string) []any {
	wanted := make(map[string]bool, len(aliases))
	for _, alias := range aliases {
		wanted[normalizeKey(alias)] = true
	}
	results := make([]any, 0, 2)
	var walk func(current any, depth int)
	walk = func(current any, depth int) {
		if depth > maxJSONDepth || len(results) >= 32 {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, value := range typed {
				if wanted[normalizeKey(key)] {
					results = append(results, value)
				}
			}
			for _, value := range typed {
				walk(value, depth+1)
			}
		case []any:
			for _, item := range typed {
				walk(item, depth+1)
			}
		}
	}
	walk(node, 0)
	return results
}

// fillUserData completes a typed snapshot from a generic payload.
func fillUserData(data *UserData, payload any) {
	if !data.HasDownload {
		if value, ok := lookupFirst(payload, "download-used", "total-download", "download", "bytes-in"); ok {
			if text, ok := toString(value); ok {
				data.DownloadText, data.DownloadBytes, data.HasDownload = text, 0, false
				if parsed, ok := parseCounter(text); ok {
					data.DownloadBytes, data.HasDownload = parsed, true
				}
			}
		}
	}
	if !data.HasUpload {
		if value, ok := lookupFirst(payload, "upload-used", "total-upload", "upload", "bytes-out"); ok {
			if text, ok := toString(value); ok {
				data.UploadText = text
				if parsed, ok := parseCounter(text); ok {
					data.UploadBytes, data.HasUpload = parsed, true
				}
			}
		}
	}
	if !data.HasUptime {
		if value, ok := lookupFirst(payload, "uptime", "uptime-used", "total-uptime", "used-time", "session-time"); ok {
			if text, ok := toString(value); ok {
				data.UptimeText = text
				if parsed, ok := parseCounter(text); ok {
					data.UptimeSeconds, data.HasUptime = parsed, true
				}
			}
		}
	}
	if !data.HasSessions {
		if value, ok := lookupFirst(payload, "activeSess", "active-sessions", "activeSessions", "sessions-count"); ok {
			if parsed, ok := toInt64(value); ok {
				data.ActiveSessions, data.HasSessions = int(parsed), true
			}
		}
	}
	if !data.HasSharedUsers {
		if value, ok := lookupFirst(payload, "sharedUsers", "shared-users", "sharedusers"); ok {
			if parsed, ok := toInt64(value); ok {
				data.SharedUsers, data.HasSharedUsers = int(parsed), true
			}
		}
	}
	if data.Username == "" {
		if value, ok := lookupFirst(payload, "username", "user", "name"); ok {
			if text, ok := toString(value); ok {
				data.Username = text
			}
		}
	}
}

// profilesFromGeneric walks a payload for profile rows of an unusual shape.
func profilesFromGeneric(payload any) []ProfileEntry {
	// A "profiles" list somewhere in the payload.
	for _, row := range collect(payload, "profiles") {
		if list, ok := row.([]any); ok {
			if entries := entriesFromList(list); len(entries) > 0 {
				return entries
			}
		}
	}
	// The payload itself may be the list of rows.
	if list, ok := payload.([]any); ok {
		if entries := entriesFromList(list); len(entries) > 0 {
			return entries
		}
	}
	// Or a wrapper object holding exactly one array of rows.
	if record, ok := payload.(map[string]any); ok {
		for _, value := range record {
			if list, ok := value.([]any); ok {
				if entries := entriesFromList(list); len(entries) > 0 {
					return entries
				}
			}
		}
	}
	return nil
}

// entriesFromList converts raw profile rows into profile entries.
func entriesFromList(list []any) []ProfileEntry {
	entries := make([]ProfileEntry, 0, len(list))
	for _, item := range list {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entry := ProfileEntry{}
		// In a user-profile row "profile" holds the plan name and "name" holds
		// the account, while the documented profiles listing uses "name" for
		// the plan, so the plan keys are tried first.
		if value, ok := lookupFirst(record, "profile", "profile-name", "plan", "name"); ok {
			if text, ok := toString(value); ok {
				entry.Name = strings.TrimSpace(text)
			}
		}
		if value, ok := lookupFirst(record, "state", "status"); ok {
			entry.State = value
			if number, ok := toInt64(value); ok {
				entry.StateValue = number
			}
		}
		if value, ok := lookupFirst(record, "expAfter", "expires-after", "end-time", "expires"); ok {
			if text, ok := toString(value); ok {
				entry.ExpAfter = strings.TrimSpace(text)
				entry.HasExpiry = entry.ExpAfter != ""
			}
		}
		if value, ok := lookupFirst(record, "id"); ok {
			if number, ok := toInt64(value); ok {
				entry.ID = int(number)
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// toInt64 converts a decoded JSON scalar into an integer when possible.
func toInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed, true
		}
		if asFloat, err := typed.Float64(); err == nil {
			return int64(asFloat), true
		}
		return 0, false
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}
		if parsed, err := strconv.ParseInt(strings.ReplaceAll(text, " ", ""), 10, 64); err == nil {
			return parsed, true
		}
		if parsed, err := strconv.ParseFloat(text, 64); err == nil {
			return int64(parsed), true
		}
		if bytesValue, _, ok := parseDataSize(text); ok {
			return bytesValue, true
		}
		if seconds, _, ok := parseTimeSpan(text); ok {
			return seconds, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// toString converts a decoded JSON scalar into a string when possible.
func toString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	case json.Number:
		return typed.String(), true
	default:
		if asInt, ok := toInt64(value); ok {
			return strconv.FormatInt(asInt, 10), true
		}
		return "", false
	}
}

// toBool converts a decoded JSON scalar into a boolean when possible.
func toBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case float64:
		return typed != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "1", "running", "active", "up":
			return true, true
		case "false", "no", "0", "used", "expired", "down", "":
			return false, true
		}
		return false, false
	default:
		return false, false
	}
}
