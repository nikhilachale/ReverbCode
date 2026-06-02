package controllers

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

// PRsController owns the /prs action routes.
type PRsController struct {
	Svc prsvc.ActionManager
}

// Register mounts the PR action routes on the supplied router.
func (c *PRsController) Register(r chi.Router) {
	r.Post("/prs/{id}/merge", c.merge)
	r.Post("/prs/{id}/resolve-comments", c.resolveComments)
}

func (c *PRsController) merge(w http.ResponseWriter, r *http.Request) {
	prID := chi.URLParam(r, "id")
	res, err := c.Svc.Merge(r.Context(), prID)
	if err != nil {
		writePRError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MergePRResponse{OK: true, PRNumber: res.PRNumber, Method: res.Method})
}

func (c *PRsController) resolveComments(w http.ResponseWriter, r *http.Request) {
	prID := chi.URLParam(r, "id")

	// Body is optional: omitting it resolves all unresolved threads.
	var in ResolveCommentsRequest
	if err := decodeJSON(r, &in); err != nil && !isEmptyBody(err) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}

	res, err := c.Svc.ResolveComments(r.Context(), prID, in.CommentIDs)
	if err != nil {
		writePRError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ResolveCommentsResponse{OK: true, Resolved: res.Resolved})
}

// isEmptyBody reports whether err signals an absent or empty request body.
// io.ErrUnexpectedEOF means a truncated/malformed body — bad request, not absent.
func isEmptyBody(err error) bool {
	return errors.Is(err, io.EOF)
}
