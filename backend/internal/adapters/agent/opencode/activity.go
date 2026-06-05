package opencode

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// DeriveActivityState maps an opencode AO hook event onto an AO activity state.
// The opencode plugin (assets/ao-activity.ts) normalizes opencode's native
// events to "session-start" / "user-prompt-submit" / "stop" before invoking
// `ao hooks opencode <event>`, so this only needs to interpret those three. The
// bool is false when the event carries no activity signal (session-start is
// metadata-only), in which case the caller reports nothing.
func DeriveActivityState(event string, _ []byte) (domain.ActivityState, bool) {
	switch event {
	case "user-prompt-submit":
		return domain.ActivityActive, true
	case "stop":
		// End of a turn: the agent is idle but alive (opencode run exits on the
		// idle event, so this is the last signal of the session).
		return domain.ActivityIdle, true
	default:
		return "", false
	}
}
