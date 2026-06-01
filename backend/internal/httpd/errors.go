package httpd

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// writeAPIError emits the locked envelope for any non-2xx response. The
// request id falls back to empty when the chi middleware hasn't tagged the
// request (e.g. in tests that bypass NewRouter).
func writeAPIError(w http.ResponseWriter, r *http.Request, status int, kind, code, message string, details map[string]any) {
	envelope.WriteAPIError(w, r, status, kind, code, message, details)
}
