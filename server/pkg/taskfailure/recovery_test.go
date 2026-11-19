package taskfailure

import "testing"

func TestEveryReasonHasARecoveryClass(t *testing.T) {
	for _, r := range AllReasons() {
		if _, ok := recoveryClassByReason[r]; !ok {
			t.Errorf("reason %q has no recovery class", r)
		}
	}
}

func TestRecoveryClassOf(t *testing.T) {
	cases := map[string]RecoveryClass{
		"agent_error.provider_usage_window":           RecoveryUsageWindow,
		"agent_error.provider_quota_limit":            RecoveryHumanAction,
		"agent_error.provider_capacity_or_rate_limit": RecoveryRateLimited,
		"agent_error.provider_server_error":           RecoveryTransient,
		"codex_semantic_inactivity":                   RecoveryFreshSession,
		"agent_blocked":                               RecoveryPermanent,
		"agent_error":                                 RecoveryUnknown,
		"":                                            RecoveryUnknown,
	}
	for reason, want := range cases {
		if got := RecoveryClassOf(reason); got != want {
			t.Errorf("RecoveryClassOf(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestClassifyUsageWindowVersusBilling(t *testing.T) {
	cases := map[string]Reason{
		"You've hit your session limit · resets 2pm (UTC)":                                ReasonAgentProviderUsageWindow,
		"You’ve hit your weekly limit · resets Nov 21, 9:00 AM":                           ReasonAgentProviderUsageWindow,
		"Claude AI usage limit reached|1795021200":                                        ReasonAgentProviderUsageWindow,
		"You've hit your usage limit. Upgrade to Pro or try again in 2 hours 13 minutes.": ReasonAgentProviderUsageWindow,
		"API Error: 403 usage limit exceeded, resets at 10:00":                            ReasonAgentProviderUsageWindow,
		// No reset named: nothing says the account recovers on its own.
		"API Error: 403 usage limit exceeded":                      ReasonAgentProviderQuotaLimit,
		"You've hit your limit":                                    ReasonAgentProviderQuotaLimit,
		"API Error: 402 insufficient_balance, try again in 1 hour": ReasonAgentProviderQuotaLimit,
		"monthly usage limit reached, resets Dec 1":                ReasonAgentProviderQuotaLimit,
		"API Error: 429 rate limit exceeded, try again in 20s":     ReasonAgentProviderCapacityOrRateLimit,
	}
	for text, want := range cases {
		if got := Classify(text); got != want {
			t.Errorf("Classify(%q) = %q, want %q", text, got, want)
		}
	}
}

// An installed daemon predating provider_usage_window reports a used-up
// subscription window as provider_quota_limit (or in a 403 bucket); the server
// must still recognise it, or the run is never retried at the reset.
func TestNormalizeDaemonReasonUpgradesUsageWindow(t *testing.T) {
	cases := []struct {
		reason, raw string
		want        Reason
	}{
		{"agent_error.provider_quota_limit", "You've hit your session limit · resets 2pm (UTC)", ReasonAgentProviderUsageWindow},
		{"agent_error.provider_quota_limit", "You've hit your usage limit. Try again in 2 hours 13 minutes.", ReasonAgentProviderUsageWindow},
		{"agent_error.provider_auth_or_access", "API Error: 403 usage limit exceeded, resets at 10:00", ReasonAgentProviderUsageWindow},
		// Billing keeps its reason: nothing says the account recovers.
		{"agent_error.provider_quota_limit", "API Error: 402 insufficient_balance", ReasonAgentProviderQuotaLimit},
		{"agent_error.provider_quota_limit", "You've hit your limit", ReasonAgentProviderQuotaLimit},
		// Only stale buckets are upgraded.
		{"agent_error.provider_network", "You've hit your session limit · resets 2pm (UTC)", ReasonAgentProviderNetwork},
	}
	for _, tc := range cases {
		if got := NormalizeDaemonReason(tc.reason, tc.raw); got != tc.want {
			t.Errorf("NormalizeDaemonReason(%q, %q) = %q, want %q", tc.reason, tc.raw, got, tc.want)
		}
	}
}
