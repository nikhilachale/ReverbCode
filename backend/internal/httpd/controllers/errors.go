package controllers

import (
	"errors"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// PR sentinel errors returned by the PR action service.
var (
	ErrPRNotFound       = errors.New("pr: not found")
	ErrPRNotMergeable   = errors.New("pr: not mergeable")
	ErrPRPreconditions  = errors.New("pr: merge preconditions unmet")
	ErrNothingToResolve = errors.New("pr: nothing to resolve")
)

// writePRError maps PR sentinel errors to their locked HTTP envelopes,
// falling back to 500 for unexpected failures.
func writePRError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrPRNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PR_NOT_FOUND", "Unknown PR", nil)
	case errors.Is(err, ErrPRNotMergeable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PR_NOT_MERGEABLE", "PR is not mergeable", nil)
	case errors.Is(err, ErrPRPreconditions):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "PR_PRECONDITIONS_UNMET", "PR merge preconditions are not met", nil)
	case errors.Is(err, ErrNothingToResolve):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "NOTHING_TO_RESOLVE", "No unresolved review threads to resolve", nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "PR_OPERATION_FAILED", "PR operation failed", nil)
	}
}
