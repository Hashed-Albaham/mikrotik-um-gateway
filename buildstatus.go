package main

import (
	"strings"
	"time"
)

// Where the plan limits of an account came from. Reported so callers know how
// much to trust the computed balances.
const (
	limitsSourceName = "profile-name-syntax"
	limitsSourceNone = "unavailable"
)

// stateExpired is the numeric profile state User Manager reports for a profile
// that has been used up ("used" in the RouterOS menu).
const stateExpired = 3

// statusResponse is the public JSON payload. Every balance is exposed twice:
// as a short human readable string for direct rendering, and as an exact number
// inside "raw" for machines.
type statusResponse struct {
	Success bool `json:"success"`

	MaskedUsername string `json:"masked_username"`
	MaskedUser     string `json:"masked_user"`

	ProfileName       string `json:"profile_name,omitempty"`
	ProfileState      string `json:"profile_state"`
	DataLeft          string `json:"data_left"`
	TotalData         string `json:"total_data"`
	TimeLeft          string `json:"time_left"`
	TotalTime         string `json:"total_time"`
	TotalValidity     string `json:"total_validity"`
	RemainingValidity string `json:"remaining_validity"`

	UsedDownload string `json:"used_download"`
	UsedUpload   string `json:"used_upload"`
	UsedUptime   string `json:"used_uptime"`

	Sessions       sessionDetails `json:"sessions"`
	ActiveSessions int            `json:"active_sessions"`
	SharedUsers    int            `json:"shared_users"`

	Raw rawValues `json:"raw"`

	LimitsSource   string   `json:"limits_source,omitempty"`
	ValidityEndsAt string   `json:"validity_ends_at,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	GeneratedAt    string   `json:"generated_at"`
}

// sessionDetails groups the session counters, matching the shape hotspot pages
// already expect.
type sessionDetails struct {
	ActiveSessions int `json:"active_sessions"`
	SharedUsers    int `json:"shared_users"`
}

// rawValues carries the exact numbers behind the human readable fields. A value
// of -1 means unlimited.
type rawValues struct {
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
}

const (
	unlimitedText = "unlimited"
	unknownText   = "unknown"
)

// buildStatus turns the portal snapshot of one account into the public payload.
func buildStatus(user string, data UserData, profiles []ProfileEntry, now time.Time) statusResponse {
	account := strings.TrimSpace(data.Username)
	if account == "" {
		account = user
	}
	masked := maskUser(account)

	response := statusResponse{
		Success:        true,
		MaskedUsername: masked,
		MaskedUser:     masked,
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		ProfileState:   "unknown",
	}

	// --- counters ------------------------------------------------------------
	usedData := data.DownloadBytes + data.UploadBytes
	if data.HasDownload || data.HasUpload {
		response.UsedDownload = data.DownloadText
		response.UsedUpload = data.UploadText
		if response.UsedDownload == "" && data.HasDownload {
			response.UsedDownload = formatBytes(data.DownloadBytes)
		}
		if response.UsedUpload == "" && data.HasUpload {
			response.UsedUpload = formatBytes(data.UploadBytes)
		}
		response.Raw.DownloadBytes = data.DownloadBytes
		response.Raw.UploadBytes = data.UploadBytes
		response.Raw.UsedDataBytes = usedData
	} else {
		response.UsedDownload = unknownText
		response.UsedUpload = unknownText
		response.Warnings = append(response.Warnings, "the portal did not report traffic counters for this account")
	}

	if data.HasUptime {
		response.UsedUptime = data.UptimeText
		if response.UsedUptime == "" {
			response.UsedUptime = formatDuration(data.UptimeSeconds)
		}
		response.Raw.UptimeSeconds = data.UptimeSeconds
	} else {
		response.UsedUptime = unknownText
	}

	response.ActiveSessions = data.ActiveSessions
	response.SharedUsers = data.SharedUsers
	response.Sessions = sessionDetails{ActiveSessions: data.ActiveSessions, SharedUsers: data.SharedUsers}
	if !data.HasSessions {
		response.Warnings = append(response.Warnings, "the portal did not report the active session count")
	}

	// --- the profile that is currently in use --------------------------------
	profile, hasProfile := selectProfile(profiles, now)
	limits := ProfileLimits{}
	limitsKnown := false
	if hasProfile {
		response.ProfileName = profile.Name
		limits = parseProfileSpec(profile.Name)
		limitsKnown = limits.ParsedFields > 0
		if profile.HasExpiry {
			if seconds, unlimited, ok := parseValidity(profile.ExpAfter, now); ok {
				response.Raw.RemainingValiditySeconds = seconds
				if unlimited {
					response.RemainingValidity = unlimitedText
				} else {
					response.RemainingValidity = formatDuration(seconds)
					if ends, ok := parseUMTimestamp(profile.ExpAfter); ok {
						response.ValidityEndsAt = ends.UTC().Format(time.RFC3339)
					}
				}
			} else {
				response.RemainingValidity = profile.ExpAfter
			}
		}
		expired := profileExpired(profile, now)
		if expired {
			response.ProfileState = "expired"
		} else {
			response.ProfileState = "active"
		}
	} else {
		response.Warnings = append(response.Warnings, "the portal did not return a profile assignment for this account")
	}

	if limitsKnown {
		response.LimitsSource = limitsSourceName
	} else {
		response.LimitsSource = limitsSourceNone
		if hasProfile {
			response.Warnings = append(response.Warnings, "the plan limits could not be resolved from the profile name; a profile name such as 1000-1G-8h is expected")
		}
	}

	// --- data balance --------------------------------------------------------
	switch {
	case limitsKnown && limits.DataUnlimit:
		response.DataLeft = unlimitedText
		response.TotalData = unlimitedText
		response.Raw.DataUnlimited = true
		response.Raw.DataLeftBytes = -1
		response.Raw.TotalDataBytes = -1
	case limitsKnown:
		remaining := limits.DataTotal - usedData
		if remaining < 0 {
			remaining = 0
		}
		response.DataLeft = formatBytes(remaining)
		response.TotalData = formatBytes(limits.DataTotal)
		response.Raw.DataLeftBytes = remaining
		response.Raw.TotalDataBytes = limits.DataTotal
	default:
		response.DataLeft = unknownText
		response.TotalData = unknownText
	}

	// --- time balance --------------------------------------------------------
	switch {
	case limitsKnown && limits.TimeUnlimit:
		response.TimeLeft = unlimitedText
		response.TotalTime = unlimitedText
		response.TotalValidity = unlimitedText
		response.Raw.TimeUnlimited = true
		response.Raw.TimeLeftSeconds = -1
		response.Raw.TotalTimeSeconds = -1
	case limitsKnown:
		remaining := limits.TimeTotal - data.UptimeSeconds
		if remaining < 0 {
			remaining = 0
		}
		response.TimeLeft = formatDuration(remaining)
		response.TotalTime = formatDuration(limits.TimeTotal)
		response.TotalValidity = response.TotalTime
		response.Raw.TimeLeftSeconds = remaining
		response.Raw.TotalTimeSeconds = limits.TimeTotal
	default:
		response.TimeLeft = unknownText
		response.TotalTime = unknownText
		response.TotalValidity = unknownText
	}

	// The portal expiry is the authoritative remaining validity; the plan
	// arithmetic above is the fallback when the portal did not send one.
	if response.RemainingValidity == "" {
		if data.HasUptime && limitsKnown && !limits.TimeUnlimit {
			remaining := limits.TimeTotal - data.UptimeSeconds
			if remaining < 0 {
				remaining = 0
			}
			response.RemainingValidity = formatDuration(remaining)
			response.Raw.RemainingValiditySeconds = remaining
		} else if response.TimeLeft != unknownText {
			response.RemainingValidity = response.TimeLeft
			response.Raw.RemainingValiditySeconds = response.Raw.TimeLeftSeconds
		} else {
			response.RemainingValidity = unknownText
		}
	}
	if response.ProfileState == "expired" {
		response.TimeLeft = "0s"
		response.RemainingValidity = "0s"
		response.Raw.TimeLeftSeconds = 0
		response.Raw.RemainingValiditySeconds = 0
	}
	return response
}

// selectProfile picks the profile that is in use. Profiles that are used up are
// skipped when a usable one exists, because a single account can hold several
// profiles and only one of them is active at a time.
func selectProfile(profiles []ProfileEntry, now time.Time) (ProfileEntry, bool) {
	if len(profiles) == 0 {
		return ProfileEntry{}, false
	}
	// Prefer a usable profile that also names a plan.
	for _, entry := range profiles {
		if entry.Name != "" && !profileExpired(entry, now) {
			return entry, true
		}
	}
	for _, entry := range profiles {
		if !profileExpired(entry, now) {
			return entry, true
		}
	}
	for _, entry := range profiles {
		if entry.Name != "" {
			return entry, true
		}
	}
	return profiles[0], true
}

// profileExpired decides whether a profile is used up: state 3 in the portal
// means "used", and an expiry that has already elapsed does the same.
func profileExpired(entry ProfileEntry, now time.Time) bool {
	switch {
	case entry.StateValue == stateExpired:
		return true
	case entry.StateValue > 0:
		return false
	}
	if text, ok := entry.State.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "used", "expired", "stopped", "disabled":
			return true
		case "running", "running active", "active":
			return false
		}
	}
	if entry.HasExpiry {
		if seconds, unlimited, ok := parseValidity(entry.ExpAfter, now); ok {
			return !unlimited && seconds <= 0
		}
	}
	return false
}
