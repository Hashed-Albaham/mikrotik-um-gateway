package main

import (
	"bytes"
	"context"
	"encoding/json"
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
const (
	apiLogin           = "/um/api/login"
	apiGetUser         = "/um/api/getUser"
	apiGetUserProfiles = "/um/api/getUserProfiles"
	apiGetProfiles     = "/um/api/getProfiles"
	apiLogout          = "/um/api/logout"
)

const (
	maxResponseBytes = 2 << 20 // 2 MiB guard against oversized portal responses
	maxJSONDepth     = 10
)

// Client is a single-use User Manager client. Every HTTP request handled by the
// gateway builds its own Client, which means its cookie jar is isolated and no
// session is ever shared or reused between callers.
type Client struct {
	baseURL string
	user    string
	http    *http.Client
}

// NewClient prepares a client with a private in-memory cookie jar.
func NewClient(baseURL string, timeout time.Duration, user string) (*Client, error) {
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
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
			},
		},
	}, nil
}

// do performs one portal call and returns the decoded JSON value.
func (c *Client) do(ctx context.Context, method, path string, payload any, query url.Values) (any, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.8")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
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
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, snippet(raw))
	}
	return decodePayload(raw), nil
}

// decodePayload accepts JSON of any shape. Non-JSON bodies are wrapped so that
// downstream lookups simply find nothing instead of failing the whole request.
func decodePayload(raw []byte) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err == nil {
		return decoded
	}
	return map[string]any{"_raw": string(trimmed)}
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// Login authenticates against the User Manager portal and stores the resulting
// session cookie in the client's private jar.
func (c *Client) Login(ctx context.Context, password string) error {
	payload := map[string]any{
		"username": c.user,
		"password": password,
		"name":     c.user,
	}
	result, err := c.do(ctx, http.MethodPost, apiLogin, payload, nil)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	if raw, found := firstOf(result, "success", "ok"); found {
		if flag, parsed := toBool(raw); parsed && !flag {
			message := "user manager rejected the credentials"
			if rawMessage, hasMessage := firstOf(result, "error-message", "error", "message"); hasMessage {
				if text, ok := toString(rawMessage); ok && text != "" {
					message = text
				}
			}
			return fmt.Errorf("login failed: %s", message)
		}
	}
	if rawMessage, hasMessage := firstOf(result, "error"); hasMessage {
		if text, ok := toString(rawMessage); ok && text != "" {
			return fmt.Errorf("login failed: %s", text)
		}
	}
	return nil
}

// FetchUser, FetchUserProfiles and FetchProfiles are the read calls that the
// gateway issues concurrently.
func (c *Client) FetchUser(ctx context.Context) (any, error) {
	query := url.Values{}
	query.Set("username", c.user)
	query.Set("name", c.user)
	return c.do(ctx, http.MethodGet, apiGetUser, nil, query)
}

func (c *Client) FetchUserProfiles(ctx context.Context) (any, error) {
	return c.do(ctx, http.MethodGet, apiGetUserProfiles, nil, nil)
}

func (c *Client) FetchProfiles(ctx context.Context) (any, error) {
	return c.do(ctx, http.MethodGet, apiGetProfiles, nil, nil)
}

// Logout invalidates the portal session using the same cookie jar.
func (c *Client) Logout(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, apiLogout, map[string]any{"username": c.user, "name": c.user}, nil)
	if err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	return nil
}

// CloseIdleConnections releases the sockets the client kept alive. It is called
// once the request lifecycle of a session is finished.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// ---------------------------------------------------------------------------
// Tolerant JSON access helpers.
//
// Portal responses differ between RouterOS builds: a value may be nested under
// a user record, a profile record or a wrapper object, and keys may be written
// as "download-used", "downloadUsed" or "download_used". The helpers below match
// keys by their normalized form and walk nested objects, so the gateway keeps
// working across those variations instead of hard coding one exact schema.
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

// firstOf returns the first value found for any of the given key aliases.
func firstOf(node any, aliases ...string) (any, bool) {
	values := collect(node, aliases...)
	if len(values) == 0 {
		return nil, false
	}
	return values[0], true
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

// findUserRecord returns the shallowest object in the payload that describes
// the requested account.
func findUserRecord(node any, user string) (map[string]any, bool) {
	target := strings.ToLower(strings.TrimSpace(user))
	if target == "" {
		return nil, false
	}
	var found map[string]any

	var walk func(current any, depth int)
	walk = func(current any, depth int) {
		if depth > maxJSONDepth || found != nil {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			for _, alias := range []string{"name", "username", "user", "login", "user-name"} {
				if value, ok := typed[alias]; ok {
					if text, ok := toString(value); ok && strings.ToLower(strings.TrimSpace(text)) == target {
						found = typed
						return
					}
				}
			}
			// A map keyed by the user name is also accepted.
			if nested, ok := typed[user]; ok {
				if asMap, ok := nested.(map[string]any); ok {
					found = asMap
					return
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
	return found, found != nil
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
		// Values may arrive with a unit, for example "1.5G" or "8h".
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

// toFloat converts a decoded JSON scalar into a float when possible.
func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		if asInt, ok := toInt64(value); ok {
			return float64(asInt), true
		}
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
		text := strings.ToLower(strings.TrimSpace(typed))
		switch text {
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
