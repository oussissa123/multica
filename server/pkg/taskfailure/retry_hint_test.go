package taskfailure

import (
	"strings"
	"testing"
	"time"
)

func TestParseRetryHint(t *testing.T) {
	now := time.Date(2026, time.November, 18, 11, 58, 0, 0, time.UTC)
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		text   string
		loc    *time.Location
		want   time.Time
		source string
	}{
		{
			name: "claude session limit with zone",
			text: "You've hit your session limit · resets 2pm (UTC)",
			want: time.Date(2026, time.November, 18, 14, 0, 0, 0, time.UTC),
		},
		{
			name: "named IANA zone",
			text: "5-hour limit reached ∙ resets 3am (Europe/Paris)",
			want: time.Date(2026, time.November, 19, 3, 0, 0, 0, paris),
		},
		{
			name: "time of day already passed rolls to tomorrow",
			text: "You've hit your session limit · resets 9am",
			want: time.Date(2026, time.November, 19, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "runtime zone used when the text names none",
			text: "You've hit your session limit · resets 2pm",
			loc:  paris,
			want: time.Date(2026, time.November, 18, 14, 0, 0, 0, paris),
		},
		{
			name: "weekly limit with date",
			text: "You've hit your weekly limit · resets Nov 21, 9:00 AM (UTC)",
			want: time.Date(2026, time.November, 21, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "claude 5-hour limit, minutes glued to pm",
			text: "You've hit your 5-hour limit · resets 4:50pm (UTC)",
			want: time.Date(2026, time.November, 18, 16, 50, 0, 0, time.UTC),
		},
		{
			name: "claude weekly limit, date and minutes glued to pm",
			text: "You’ve hit your weekly limit · resets Nov 24, 4:50pm (UTC)",
			want: time.Date(2026, time.November, 24, 16, 50, 0, 0, time.UTC),
		},
		{
			name: "24h clock",
			text: "usage limit reached, try again at 15:30",
			want: time.Date(2026, time.November, 18, 15, 30, 0, 0, time.UTC),
		},
		{
			name: "codex relative",
			text: "You've hit your usage limit. Upgrade to Pro or try again in 2 hours 13 minutes.",
			want: now.Add(2*time.Hour + 13*time.Minute),
		},
		{
			name: "compact relative",
			text: "rate limited, retry in 1h 30m",
			want: now.Add(90 * time.Minute),
		},
		{
			name: "retry-after header seconds",
			text: "API Error: 429 Too Many Requests (retry-after: 120)",
			want: now.Add(2 * time.Minute),
		},
		{
			name:   "epoch form",
			text:   "Claude AI usage limit reached|1795021200",
			want:   time.Unix(1795021200, 0),
			source: "error_epoch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryHint(tc.text, now, tc.loc)
			if !ok {
				t.Fatalf("ParseRetryHint(%q) found no hint", tc.text)
			}
			if !got.At.Equal(tc.want) {
				t.Fatalf("ParseRetryHint(%q) = %s, want %s", tc.text, got.At, tc.want)
			}
			wantSource := tc.source
			if wantSource == "" {
				wantSource = "error_text"
			}
			if got.Source != wantSource {
				t.Fatalf("source = %q, want %q", got.Source, wantSource)
			}
		})
	}
}

func TestParseRetryHintRejects(t *testing.T) {
	now := time.Date(2026, time.November, 18, 11, 58, 0, 0, time.UTC)
	for _, text := range []string{
		"",
		"insufficient_balance: please top up",
		"the counter resets 5 times per run",
		"resets 25:00",
		"Claude AI usage limit reached|1000000000",                   // in the past
		"try again in 30 days",                                       // beyond any provider window
		"You’ve hit your weekly limit · resets Nov 27, 4:50pm (UTC)", // past the longest window
		"API Error: 500 Internal Server Error",
		strings.Repeat("x", retryHintMaxInput) + " try again in 5 minutes",
	} {
		if got, ok := ParseRetryHint(text, now, nil); ok {
			t.Errorf("ParseRetryHint(%q) = %s, want no hint", text, got.At)
		}
	}
}
