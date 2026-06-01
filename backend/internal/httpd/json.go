package httpd

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// writeJSON serialises v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	envelope.WriteJSON(w, status, v)
}
