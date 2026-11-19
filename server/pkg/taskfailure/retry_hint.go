package taskfailure

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// retryHintMaxInput bounds how much error text the parser reads. Provider
// limit notices are one line; a longer input is a stderr tail whose head
// carries the notice.
const retryHintMaxInput = 8 << 10

// retryHintMaxAhead rejects parsed times further away than any provider
// window (the longest is weekly), which only a misread produces.
const retryHintMaxAhead = 8 * 24 * time.Hour

var (
	// "try again in 2 hours 13 minutes", "retry after 120 seconds",
	// "try again in 1h 30m", "retry-after: 120". Captures the duration text
	// that follows; a bare number is seconds, as in the HTTP header.
	retryRelativeRe = regexp.MustCompile(`(?:try again in|retry after|retry in|retry-after:?)\s+((?:\d+\s*[a-z]*[\s,]*(?:and\s+)?){1,4})`)
	durationPartRe  = regexp.MustCompile(`(\d+)\s*([a-z]*)`)

	// "resets 2pm", "resets at 3:45 pm (europe/paris)", "try again at 15:00",
	// "resets nov 21, 9:00 am (utc)". Group 1: optional month+day, 2: hour,
	// 3: minutes, 4: am/pm, 5: zone.
	retryAbsoluteRe = regexp.MustCompile(`(?:resets|reset at|try again at)\s+(?:at\s+)?(?:on\s+)?((?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]{1,64})\))?`)

	// Claude Code's machine-readable form: "Claude AI usage limit reached|1763485200".
	retryEpochRe = regexp.MustCompile(`limit reached\|(\d{10})`)

	monthByPrefix = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
		"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
		"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
	}
)

// RetryHint is the moment a provider says a limit lifts.
type RetryHint struct {
	At time.Time
	// Source names what the time was read from: "error_text" for prose,
	// "error_epoch" for a machine-readable timestamp.
	Source string
}

// ParseRetryHint reads the time a provider limit lifts from an error message.
// loc resolves wall-clock times that name no zone; nil means UTC. A time of
// day without a date is its next occurrence after now. It reports false when
// the text names no time, or names one in the past or implausibly far ahead.
func ParseRetryHint(text string, now time.Time, loc *time.Location) (RetryHint, bool) {
	if len(text) > retryHintMaxInput {
		text = text[:retryHintMaxInput]
	}
	if loc == nil {
		loc = time.UTC
	}
	lower := strings.ToLower(text)

	if m := retryEpochRe.FindStringSubmatch(lower); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if hint, ok := plausibleHint(time.Unix(sec, 0), now, "error_epoch"); ok {
				return hint, true
			}
		}
	}
	if m := retryAbsoluteRe.FindStringSubmatch(lower); m != nil {
		if at, ok := absoluteResetTime(m, now, loc); ok {
			if hint, ok := plausibleHint(at, now, "error_text"); ok {
				return hint, true
			}
		}
	}
	if m := retryRelativeRe.FindStringSubmatch(lower); m != nil {
		if d, ok := parseDurationWords(m[1]); ok {
			return plausibleHint(now.Add(d), now, "error_text")
		}
	}
	return RetryHint{}, false
}

func plausibleHint(at, now time.Time, source string) (RetryHint, bool) {
	if !at.After(now) || at.Sub(now) > retryHintMaxAhead {
		return RetryHint{}, false
	}
	return RetryHint{At: at, Source: source}, true
}

func absoluteResetTime(m []string, now time.Time, loc *time.Location) (time.Time, bool) {
	hour, err := strconv.Atoi(m[2])
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if m[3] != "" {
		if minute, err = strconv.Atoi(m[3]); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	switch m[4] {
	case "am":
		if hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		if hour != 12 {
			hour += 12
		}
	default:
		// A bare number is a time only in 24h form with minutes ("15:00");
		// "resets 3 times" or "resets 5" are not times.
		if m[3] == "" || hour > 23 {
			return time.Time{}, false
		}
	}
	if zone := strings.TrimSpace(m[5]); zone != "" {
		if z, ok := resolveZone(zone); ok {
			loc = z
		}
	}
	local := now.In(loc)
	if m[1] != "" {
		month, day, ok := parseMonthDay(m[1])
		if !ok {
			return time.Time{}, false
		}
		at := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
		if at.Before(now.Add(-24 * time.Hour)) {
			at = at.AddDate(1, 0, 0)
		}
		return at, true
	}
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

func resolveZone(zone string) (*time.Location, bool) {
	switch zone {
	case "utc", "gmt", "z":
		return time.UTC, true
	}
	// IANA names are case-sensitive ("Europe/Paris"); the input was lowercased.
	parts := strings.Split(zone, "/")
	for i, p := range parts {
		words := strings.Split(p, "_")
		for j, w := range words {
			if w != "" {
				words[j] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		parts[i] = strings.Join(words, "_")
	}
	z, err := time.LoadLocation(strings.Join(parts, "/"))
	if err != nil {
		return nil, false
	}
	return z, true
}

func parseMonthDay(s string) (time.Month, int, bool) {
	fields := strings.Fields(strings.NewReplacer(",", " ", ".", " ").Replace(s))
	if len(fields) < 2 || len(fields[0]) < 3 {
		return 0, 0, false
	}
	month, ok := monthByPrefix[fields[0][:3]]
	if !ok {
		return 0, 0, false
	}
	day, err := strconv.Atoi(strings.TrimRight(fields[1], "stndrh"))
	if err != nil || day < 1 || day > 31 {
		return 0, 0, false
	}
	return month, day, true
}

func parseDurationWords(s string) (time.Duration, bool) {
	var total time.Duration
	matched := false
	for _, part := range durationPartRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(part[1])
		if err != nil {
			return 0, false
		}
		var unit time.Duration
		switch u := part[2]; {
		case u == "" || u == "s" || strings.HasPrefix(u, "sec"):
			unit = time.Second
		case u == "m" || strings.HasPrefix(u, "min"):
			unit = time.Minute
		case u == "h" || strings.HasPrefix(u, "hour") || strings.HasPrefix(u, "hr"):
			unit = time.Hour
		case u == "d" || strings.HasPrefix(u, "day"):
			unit = 24 * time.Hour
		case u == "ms" || strings.HasPrefix(u, "millisecond"):
			unit = time.Millisecond
		default:
			continue
		}
		total += time.Duration(n) * unit
		matched = true
	}
	return total, matched && total > 0
}
