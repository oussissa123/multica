package taskfailure

// RecoveryClass groups failure reasons by what it takes for the work to go on.
// It describes the failure; whether a given reason is retried automatically,
// and how often, is the server's retry policy (service.retryableReasons).
type RecoveryClass string

const (
	// RecoveryTransient: a brief infrastructure or provider blip; the same
	// request is likely to succeed shortly.
	RecoveryTransient RecoveryClass = "transient"
	// RecoveryRateLimited: the provider is throttling requests; retry after
	// a backoff that honours any retry-after it gave.
	RecoveryRateLimited RecoveryClass = "rate_limited"
	// RecoveryUsageWindow: a subscription usage window is used up and resets
	// on its own; retry once it has reset.
	RecoveryUsageWindow RecoveryClass = "usage_window"
	// RecoveryWaitForRuntime: the runtime is gone or reconnecting.
	RecoveryWaitForRuntime RecoveryClass = "wait_for_runtime"
	// RecoveryFreshSession: the agent conversation is poisoned; only a new
	// session can make progress.
	RecoveryFreshSession RecoveryClass = "fresh_session"
	// RecoveryHumanAction: a person has to fix credentials, configuration,
	// billing or the local install first.
	RecoveryHumanAction RecoveryClass = "human_action"
	// RecoveryPermanent: the agent's own decision, or a failure that
	// repeating cannot change.
	RecoveryPermanent RecoveryClass = "permanent"
	// RecoveryUnknown: the classifier could not tell.
	RecoveryUnknown RecoveryClass = "unknown"
)

var recoveryClassByReason = map[Reason]RecoveryClass{
	ReasonQueuedExpired:             RecoveryWaitForRuntime,
	ReasonRuntimeOffline:            RecoveryWaitForRuntime,
	ReasonRuntimeReconnectTimeout:   RecoveryWaitForRuntime,
	ReasonRuntimeRecovery:           RecoveryTransient,
	ReasonTimeout:                   RecoveryTransient,
	ReasonProcessCleanupUnconfirmed: RecoveryPermanent,
	ReasonProcessCleanupBlocked:     RecoveryPermanent,
	ReasonIterationLimit:            RecoveryFreshSession,
	ReasonAgentBlocked:              RecoveryPermanent,
	ReasonAPIInvalidRequest:         RecoveryFreshSession,
	ReasonSkillBundleUnavailable:    RecoveryTransient,
	// A local CLI that missed its preparation deadline misses it again on
	// the same host; someone has to look at the machine.
	ReasonRuntimeCLITimeout:        RecoveryHumanAction,
	ReasonEnvironmentPrepareFailed: RecoveryHumanAction,
	ReasonInvalidTaskIdentity:      RecoveryPermanent,
	ReasonRuntimeAccessDenied:      RecoveryHumanAction,
	ReasonRuntimeAccessRevoked:     RecoveryHumanAction,
	// The project's linked folder is gone; someone has to restore or relink it.
	ReasonLocalDirectoryMissing: RecoveryHumanAction,

	ReasonAgentProviderAuthOrAccess:        RecoveryHumanAction,
	ReasonAgentProviderQuotaLimit:          RecoveryHumanAction,
	ReasonAgentProviderUsageWindow:         RecoveryUsageWindow,
	ReasonAgentProviderCapacityOrRateLimit: RecoveryRateLimited,
	ReasonAgentProviderServerError:         RecoveryTransient,
	ReasonAgentProviderNetwork:             RecoveryTransient,

	// The run may have executed tools before these; with no idempotency key
	// a replay is not safe, so they are terminal.
	ReasonAgentProcessFailure:           RecoveryPermanent,
	ReasonAgentEmptyOrUnparseableOutput: RecoveryPermanent,
	ReasonAgentTimeout:                  RecoveryPermanent,
	ReasonAgentContextOverflow:          RecoveryFreshSession,

	ReasonAgentMissingConfig:              RecoveryHumanAction,
	ReasonAgentModelNotFoundOrUnavailable: RecoveryHumanAction,
	ReasonAgentRuntimeVersionUnsupported:  RecoveryHumanAction,
	ReasonAgentRuntimeMissingExecutable:   RecoveryHumanAction,

	ReasonAgentUnknown: RecoveryUnknown,
}

// operationalRecoveryClasses covers daemon reasons outside the canonical
// taxonomy (see daemon/poisoned.go); all of them poison the session.
var operationalRecoveryClasses = map[string]RecoveryClass{
	"agent_fallback_message":    RecoveryFreshSession,
	"codex_semantic_inactivity": RecoveryFreshSession,
	"codex_resume_oversized":    RecoveryFreshSession,
	"opencode_clock_skew":       RecoveryFreshSession,
}

// RecoveryClassOf returns the recovery class of a persisted failure_reason.
// Unrecognised values, including legacy coarse ones, are RecoveryUnknown.
func RecoveryClassOf(reason string) RecoveryClass {
	if c, ok := recoveryClassByReason[Reason(reason)]; ok {
		return c
	}
	if c, ok := operationalRecoveryClasses[reason]; ok {
		return c
	}
	return RecoveryUnknown
}
