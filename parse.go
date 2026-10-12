package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Quantity is a metric that is reported both as a raw machine readable value
// and as a short human readable string. The Unit field tells the caller how to
// interpret Raw: "bytes" for data volumes and "seconds" for durations.
type Quantity struct {
	Unit      string `json:"unit"`
	Raw       int64  `json:"raw"`
	Human     string `json:"human"`
	Unlimited bool   `json:"unlimited"`
}

// ProfileLimits holds the plan limits encoded in a User Manager profile name,
// for example "1000-1G-8h" (price - data - time) or "1000-un-8h" (unlimited data).
type ProfileLimits struct {
	Raw          string
	Price        float64
	HasPrice     bool
	DataTotal    int64
	DataUnlimit  bool
	TimeTotal    int64 // seconds
	TimeUnlimit  bool
	ParsedFields int
}

const (
	kib int64 = 1 << 10
	mib int64 = 1 << 20
	gib int64 = 1 << 30
	tib int64 = 1 << 40
)

var unlimitedTokens = map[string]bool{
	"un": true, "unl": true, "unltd": true, "unlimited": true, "inf": true, "infinite": true,
}

// maskUser hides the middle of an account identifier so that the gateway never
// echoes a plain user name back to the caller. "0018067158" becomes "00******58".
// Identifiers of four characters or fewer are fully masked.
func maskUser(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	runes := []rune(name)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:2]) + strings.Repeat("*", len(runes)-4) + string(runes[len(runes)-2:])
}

// formatBytes renders a byte count using binary units, for example "550 MB".
func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n >= tib:
		return trimFloat(float64(n)/float64(tib)) + " TB"
	case n >= gib:
		return trimFloat(float64(n)/float64(gib)) + " GB"
	case n >= mib:
		return trimFloat(float64(n)/float64(mib)) + " MB"
	case n >= kib:
		return trimFloat(float64(n)/float64(kib)) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

func trimFloat(v float64) string {
	if v >= 100 {
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// formatDuration renders a duration in seconds in a compact form such as
// "2h 15m", "45s" or "3d 4h".
func formatDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds == 0 {
		return "0s"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60

	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, strconv.FormatInt(days, 10)+"d")
	}
	if hours > 0 {
		parts = append(parts, strconv.FormatInt(hours, 10)+"h")
	}
	if minutes > 0 {
		parts = append(parts, strconv.FormatInt(minutes, 10)+"m")
	}
	if secs > 0 && len(parts) < 2 {
		parts = append(parts, strconv.FormatInt(secs, 10)+"s")
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// bytesQuantity builds a byte based metric.
func bytesQuantity(raw int64, unlimited bool) Quantity {
	if unlimited {
		return Quantity{Unit: "bytes", Raw: -1, Human: "unlimited", Unlimited: true}
	}
	if raw < 0 {
		raw = 0
	}
	return Quantity{Unit: "bytes", Raw: raw, Human: formatBytes(raw)}
}

// secondsQuantity builds a duration based metric.
func secondsQuantity(raw int64, unlimited bool) Quantity {
	if unlimited {
		return Quantity{Unit: "seconds", Raw: -1, Human: "unlimited", Unlimited: true}
	}
	if raw < 0 {
		raw = 0
	}
	return Quantity{Unit: "seconds", Raw: raw, Human: formatDuration(raw)}
}

// parseDataSize converts a plan token such as "1G", "500MB" or "un" into bytes.
// It returns ok=false when the token is not a data token at all.
func parseDataSize(token string) (bytes int64, unlimited bool, ok bool) {
	t := normalizeToken(token)
	if t == "" {
		return 0, false, false
	}
	if unlimitedTokens[t] {
		return 0, true, true
	}
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, false, false
	}
	number, err := strconv.ParseFloat(t[:i], 64)
	if err != nil {
		return 0, false, false
	}
	suffix := strings.TrimSpace(t[i:])
	multiplier := int64(0)
	switch suffix {
	case "":
		return 0, false, false // a bare number is not a data token
	case "b":
		multiplier = 1
	case "k", "kb", "kib":
		multiplier = kib
	case "m", "mb", "mib":
		multiplier = mib
	case "g", "gb", "gib":
		multiplier = gib
	case "t", "tb", "tib":
		multiplier = tib
	default:
		return 0, false, false
	}
	return int64(math.Round(number * float64(multiplier))), false, true
}

// parseTimeSpan converts a plan token such as "8h", "1d12h", "30m" or "00:10:00"
// into seconds. It returns ok=false when the token is not a duration token.
func parseTimeSpan(token string) (seconds int64, unlimited bool, ok bool) {
	t := normalizeToken(token)
	if t == "" {
		return 0, false, false
	}
	if unlimitedTokens[t] {
		return 0, true, true
	}
	if strings.Count(t, ":") == 2 {
		parts := strings.Split(t, ":")
		h, err1 := strconv.ParseInt(parts[0], 10, 64)
		m, err2 := strconv.ParseInt(parts[1], 10, 64)
		s, err3 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || m > 59 || s > 59 {
			return 0, false, false
		}
		return h*3600 + m*60 + s, false, true
	}

	total := int64(0)
	matched := false
	number := int64(0)
	hasNumber := false
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			number = number*10 + int64(r-'0')
			hasNumber = true
		case r == 'w' || r == 'd' || r == 'h' || r == 'm' || r == 's':
			if !hasNumber {
				return 0, false, false
			}
			switch r {
			case 'w':
				total += number * 604800
			case 'd':
				total += number * 86400
			case 'h':
				total += number * 3600
			case 'm':
				total += number * 60
			case 's':
				total += number
			}
			number, hasNumber, matched = 0, false, true
		default:
			return 0, false, false
		}
	}
	if hasNumber || !matched {
		return 0, false, false
	}
	return total, false, true
}

// parseProfileSpec reads the plan limits out of a User Manager profile name.
//
// The gateway does not assume a fixed field order: every token separated by
// "-", "_" or whitespace is classified on its own. Tokens that carry a data
// suffix ("1G") are treated as the data allowance, tokens that look like a
// duration ("8h", "00:10:00") as the time allowance, tokens that are a bare
// number as the price, and the tokens "un"/"unlimited"/"inf" as unlimited.
// A plan such as "1000-1G-8h" therefore resolves to price 1000, 1 GiB of data
// and 8 hours of uptime.
func parseProfileSpec(name string) ProfileLimits {
	limits := ProfileLimits{Raw: strings.TrimSpace(name)}
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '/'
	})
	for _, field := range fields {
		if bytes, unlimited, ok := parseDataSize(field); ok && !limits.DataUnlimit && limits.DataTotal == 0 {
			limits.DataUnlimit = unlimited
			limits.DataTotal = bytes
			limits.ParsedFields++
			continue
		}
		if secs, unlimited, ok := parseTimeSpan(field); ok && !limits.TimeUnlimit && limits.TimeTotal == 0 {
			limits.TimeUnlimit = unlimited
			limits.TimeTotal = secs
			limits.ParsedFields++
			continue
		}
		if !limits.HasPrice {
			if price, err := strconv.ParseFloat(strings.TrimSpace(field), 64); err == nil {
				limits.Price = price
				limits.HasPrice = true
			}
		}
	}
	return limits
}

// normalizeToken lowercases a plan token and strips separators and a trailing
// "/s" style suffix so that "1 GB", "1gb" and "1G" are all recognised.
func normalizeToken(token string) string {
	t := strings.ToLower(strings.TrimSpace(token))
	t = strings.ReplaceAll(t, " ", "")
	t = strings.ReplaceAll(t, ",", "")
	t = strings.ReplaceAll(t, "/s", "")
	return t
}

// parseValidity interprets the remaining validity reported by the portal. The
// value is either a formatted duration ("2d 5h"), an absolute timestamp, the
// word unlimited, or "0"/"-" for a profile that is already used up.
func parseValidity(value string, now time.Time) (seconds int64, unlimited bool, ok bool) {
	text := strings.TrimSpace(value)
	if text == "" || text == "..." {
		return 0, false, false
	}
	if unlimitedTokens[normalizeToken(text)] {
		return 0, true, true
	}
	if text == "-" {
		return 0, false, true
	}
	normalized := normalizeToken(text)
	if normalized == "0" || normalized == "0s" || normalized == "00:00:00" {
		return 0, false, true
	}
	if timestamp, ok := parseUMTimestamp(text); ok {
		delta := int64(timestamp.Sub(now).Seconds())
		if delta < 0 {
			delta = 0
		}
		return delta, false, true
	}
	if seconds, _, ok := parseTimeSpan(text); ok {
		return seconds, false, true
	}
	return 0, false, false
}

// parseUMTimestamp reads a User Manager timestamp such as
// "2026-10-12 22:00:00" or RFC3339 with an optional offset.
func parseUMTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"Jan/02/2006 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if ts, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
