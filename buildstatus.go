package main

import (
	"strings"
	"time"
)

// portalData carries the three concurrent User Manager responses.
type portalData struct {
	user         any // GET /um/api/getUser
	userProfiles any // GET /um/api/getUserProfiles
	profiles     any // GET /um/api/getProfiles
}

// Where the plan limits of an account came from. Reported so callers know how
// much to trust the computed balances.
const (
	limitsSourceRecord = "profile-record-limits"
	limitsSourceName   = "profile-name-syntax"
	limitsSourceNone   = "unavailable"
)

// buildStatus turns the raw portal responses of one account into the public
// payload: used versus allowed data, remaining uptime, validity and state.
func buildStatus(user string, data portalData, now time.Time) statusResponse {
	response := statusResponse{
		Success:     true,
		MaskedUser:  maskUser(user),
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}

	userRecord, hasUser := findUserRecord(data.user, user)
	if !hasUser {
		userRecord, hasUser = findUserRecord(data.userProfiles, user)
	}
	if !hasUser {
		response.Warnings = append(response.Warnings, "the portal did not return a record for this account")
	}
	if userRecord == nil {
		userRecord = map[string]any{}
	}

	profileRecord, hasProfileRecord := findProfileRecord(data.userProfiles, user)
	profileName := resolveProfileName(userRecord, profileRecord)
	if profileName != "" {
		response.Profile = profileName
	}

	// --- usage ---------------------------------------------------------------
	downloadUsed, hasDownload := usageValue(userRecord, data.user, "download-used", "downloads", "total-download", "download", "bytes-in")
	uploadUsed, hasUpload := usageValue(userRecord, data.user, "upload-used", "uploads", "total-upload", "upload", "bytes-out")
	if !hasDownload && !hasUpload {
		response.Warnings = append(response.Warnings, "the portal did not report traffic counters for this account")
	}
	dataUsed := downloadUsed + uploadUsed
	if dataUsed > 0 {
		quantity := bytesQuantity(dataUsed, false)
		response.DataUsed = &quantity
	}

	uptimeUsed, hasUptime := usageValue(userRecord, data.user, "uptime", "uptime-used", "total-uptime", "time-used", "used-time", "session-time")
	if hasUptime {
		quantity := secondsQuantity(uptimeUsed, false)
		response.TimeUsed = &quantity
	}

	// --- plan limits ---------------------------------------------------------
	limits, source, limitsKnown := resolveLimits(profileRecord, profileName)
	response.LimitsSource = source
	if !limitsKnown {
		response.Warnings = append(response.Warnings, "the plan limits of this profile could not be resolved; profile name should look like 1000-1G-8h")
	}

	// --- data balance --------------------------------------------------------
	switch {
	case limitsKnown && limits.DataUnlimit:
		response.DataLeft = bytesQuantity(0, true)
	case limitsKnown:
		remaining := limits.DataTotal - dataUsed
		if remaining < 0 {
			remaining = 0
		}
		response.DataLeft = bytesQuantity(remaining, false)
	default:
		response.DataLeft = Quantity{Unit: "bytes", Raw: 0, Human: "unknown"}
	}

	// --- validity ------------------------------------------------------------
	endTime, hasEndTime := recordTime(profileRecord, "end-time", "ends-at", "expires", "expiry", "valid-until")
	if hasEndTime {
		response.ValidityEndsAt = endTime.UTC().Format(time.RFC3339)
	}

	totalValidityKnown := limitsKnown
	if totalValidityKnown {
		response.TotalValidity = secondsQuantity(limits.TimeTotal, limits.TimeUnlimit)
	} else {
		response.TotalValidity = Quantity{Unit: "seconds", Raw: 0, Human: "unknown"}
	}

	switch {
	case limitsKnown && limits.TimeUnlimit:
		response.TimeLeft = secondsQuantity(0, true)
		response.RemainingValidity = secondsQuantity(0, true)
	case hasEndTime:
		seconds := int64(endTime.Sub(now).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		response.RemainingValidity = secondsQuantity(seconds, false)
		response.TimeLeft = secondsQuantity(seconds, false)
	case limitsKnown:
		remaining := limits.TimeTotal - uptimeUsed
		if remaining < 0 {
			remaining = 0
		}
		response.TimeLeft = secondsQuantity(remaining, false)
		response.RemainingValidity = secondsQuantity(remaining, false)
	default:
		response.TimeLeft = Quantity{Unit: "seconds", Raw: 0, Human: "unknown"}
		response.RemainingValidity = Quantity{Unit: "seconds", Raw: 0, Human: "unknown"}
	}

	// --- state and sessions --------------------------------------------------
	state, hasState := recordString(profileRecord, "state", "status", "profile-state")
	response.ProfileState = normalizeProfileState(state)
	if !hasState || response.ProfileState == "unknown" {
		switch {
		case hasEndTime && endTime.Before(now):
			response.ProfileState = "expired"
		case hasEndTime:
			response.ProfileState = "active"
		}
	}
	if response.ProfileState == "unknown" {
		response.Warnings = append(response.Warnings, "the profile state was not reported by the portal")
	}

	sessions, hasSessions := sessionCount(data.user, data.userProfiles)
	if hasSessions {
		response.ActiveSessions = sessions
	} else {
		response.Warnings = append(response.Warnings, "the active session count was not reported by the portal")
	}

	if !hasProfileRecord {
		response.Warnings = append(response.Warnings, "no user-profile assignment record was found for this account")
	}
	return response
}

// usageValue reads a counter from the user record, then from the whole user
// payload as a fallback, because some builds nest counters under a "monitor"
// sub-object.
func usageValue(record map[string]any, payload any, aliases ...string) (int64, bool) {
	if record != nil {
		if value, ok := firstOf(record, aliases...); ok {
			if parsed, ok := toInt64(value); ok {
				return parsed, true
			}
		}
	}
	if value, ok := firstOf(payload, aliases...); ok {
		if parsed, ok := toInt64(value); ok {
			return parsed, true
		}
	}
	return 0, false
}

// resolveProfileName finds the profile currently assigned to the account.
func resolveProfileName(userRecord, profileRecord map[string]any) string {
	aliases := []string{"actual-profile", "profile", "user-profile", "profile-name", "actualprofile"}
	if name, ok := recordString(userRecord, aliases...); ok && name != "" {
		return name
	}
	if name, ok := recordString(profileRecord, aliases...); ok && name != "" {
		return name
	}
	return ""
}

// resolveLimits determines the plan allowance. Explicit numeric limits in the
// profile record win; otherwise the limits encoded in the profile name are used.
// RouterOS stores 0 in limit fields when no limit is configured, so zero values
// are treated as "not set" and fall through to the name syntax.
func resolveLimits(profileRecord map[string]any, profileName string) (ProfileLimits, string, bool) {
	if profileRecord != nil {
		limits := ProfileLimits{Raw: profileName}
		known := false

		if total, unlimited, ok := positiveLimit(profileRecord, "transfer-limit", "total-limit", "data-limit", "bytes-limit"); ok {
			limits.DataTotal, limits.DataUnlimit, known = total, unlimited, true
		} else if down, _, okDown := positiveLimit(profileRecord, "download-limit"); okDown {
			up, _, okUp := positiveLimit(profileRecord, "upload-limit")
			if okUp {
				limits.DataTotal = down + up
			} else {
				limits.DataTotal = down
			}
			known = true
		}

		if total, unlimited, ok := positiveLimit(profileRecord, "validity", "uptime-limit", "time-limit", "limit-uptime"); ok {
			limits.TimeTotal, limits.TimeUnlimit, known = total, unlimited, true
		}

		if known {
			if price, ok := toFloat(anyOf(profileRecord, "price")); ok {
				limits.Price, limits.HasPrice = price, true
			}
			return limits, limitsSourceRecord, true
		}
	}

	parsed := parseProfileSpec(profileName)
	if parsed.ParsedFields > 0 {
		return parsed, limitsSourceName, true
	}
	return ProfileLimits{Raw: profileName}, limitsSourceNone, false
}

// positiveLimit reads one limit field and treats 0 and empty values as unset.
func positiveLimit(record map[string]any, aliases ...string) (int64, bool, bool) {
	value, ok := firstOf(record, aliases...)
	if !ok {
		return 0, false, false
	}
	if text, ok := value.(string); ok && unlimitedTokens[normalizeToken(text)] {
		return 0, true, true
	}
	parsed, ok := toInt64(value)
	if !ok || parsed <= 0 {
		return 0, false, false
	}
	return parsed, false, true
}

func anyOf(record map[string]any, aliases ...string) any {
	value, _ := firstOf(record, aliases...)
	return value
}

// recordString returns the first string value for the given key aliases.
func recordString(record map[string]any, aliases ...string) (string, bool) {
	if record == nil {
		return "", false
	}
	value, ok := firstOf(record, aliases...)
	if !ok {
		return "", false
	}
	text, ok := toString(value)
	return strings.TrimSpace(text), ok
}

// recordTime parses the first parseable timestamp among the key aliases.
func recordTime(record map[string]any, aliases ...string) (time.Time, bool) {
	if record == nil {
		return time.Time{}, false
	}
	values := collect(record, aliases...)
	for _, value := range values {
		text, ok := toString(value)
		if !ok {
			continue
		}
		if parsed, ok := parseUMTimestamp(text); ok {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// normalizeProfileState maps the User Manager states onto the two states the
// API contract exposes: active and expired.
func normalizeProfileState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running active", "running-active", "running", "active":
		return "active"
	case "used", "expired", "stopped", "disabled":
		return "expired"
	default:
		return "unknown"
	}
}

// findProfileRecord locates the user-profile assignment row of an account.
func findProfileRecord(payload any, user string) (map[string]any, bool) {
	record, ok := findUserRecord(payload, user)
	if !ok {
		return nil, false
	}
	if _, ok := firstOf(record, "profile", "user-profile", "profile-name"); !ok {
		return nil, false
	}
	return record, true
}

// sessionCount prefers an explicit counter and falls back to counting the
// session rows that are marked active.
func sessionCount(payloads ...any) (int, bool) {
	for _, payload := range payloads {
		if value, ok := firstOf(payload, "active-sessions", "active-session-count", "activeSessions", "sessions-count"); ok {
			if parsed, ok := toInt64(value); ok {
				return int(parsed), true
			}
		}
	}
	for _, payload := range payloads {
		if count, ok := countActiveSessionRows(payload); ok {
			return count, true
		}
	}
	return 0, false
}

// countActiveSessionRows walks the payload for a list of session objects and
// counts those whose active flag is set.
func countActiveSessionRows(payload any) (int, bool) {
	found := false
	count := 0

	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		if depth > maxJSONDepth || node == nil {
			return
		}
		switch typed := node.(type) {
		case []any:
			for _, item := range typed {
				record, ok := item.(map[string]any)
				if !ok {
					walk(item, depth+1)
					continue
				}
				value, ok := firstOf(record, "active", "is-active")
				if !ok {
					walk(record, depth+1)
					continue
				}
				found = true
				if flag, ok := toBool(value); ok && flag {
					count++
				}
			}
		case map[string]any:
			for _, value := range typed {
				walk(value, depth+1)
			}
		}
	}
	walk(payload, 0)
	return count, found
}
