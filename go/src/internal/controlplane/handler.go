package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"presence-tracker/src/internal/session"
)

// ActiveMeetingID is the alias clients use when they do not name a meeting
// explicitly. The Sessions implementation resolves it to the single active
// session, or returns ErrNoActiveSession / ErrAmbiguousSession.
const ActiveMeetingID = "active"

// Sentinel errors that Sessions implementations may return from Resolve.
var (
	ErrNoActiveSession  = errors.New("controlplane: no active session")
	ErrAmbiguousSession = errors.New("controlplane: multiple active sessions; specify --meeting=<id>")
	ErrMeetingNotFound  = errors.New("controlplane: meeting not found")
)

// Sessions looks up the running tracker session for a meeting ID.
type Sessions interface {
	// Resolve returns the coordinator for the given meetingID. If
	// meetingID == ActiveMeetingID, it returns the single active
	// coordinator (or ErrNoActiveSession / ErrAmbiguousSession).
	Resolve(meetingID string) (*session.Coordinator, error)
}

// Mount registers the control-plane routes on mux.
func Mount(mux *http.ServeMux, sessions Sessions) {
	h := &handler{sessions: sessions}
	mux.HandleFunc("POST /meetings/{id}/polls", h.triggerPoll)
}

// PublishPort exports PTRACK_PORT to the current process environment so any
// child it spawns (and any ptrack poll invocation those children make) finds
// its way back to the same daemon. Safe to call once at startup.
func PublishPort(port int) error {
	return os.Setenv("PTRACK_PORT", strconv.Itoa(port))
}

type handler struct {
	sessions Sessions
}

type pollRequest struct {
	Type     string `json:"type"`
	BankPath string `json:"bank_path"`
}

type pollResponse struct {
	PollID         string `json:"poll_id"`
	ScheduledCount int    `json:"scheduled_count"`
	SkippedCount   int    `json:"skipped_count"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *handler) triggerPoll(w http.ResponseWriter, r *http.Request) {
	meetingID := r.PathValue("id")

	var req pollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.BankPath == "" {
		writeError(w, http.StatusBadRequest, "bank_path is required")
		return
	}
	if req.Type == "" {
		req.Type = "custom"
	}

	coord, err := h.sessions.Resolve(meetingID)
	switch {
	case errors.Is(err, ErrNoActiveSession):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrAmbiguousSession):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrMeetingNotFound):
		writeError(w, http.StatusConflict, fmt.Sprintf("no active session for meeting %q", meetingID))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	result, err := coord.RunPoll(r.Context(), req.BankPath, req.Type)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		// Anything else from Load is a validation failure.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, pollResponse{
		PollID:         result.PollID,
		ScheduledCount: result.ScheduledCount,
		SkippedCount:   result.SkippedCount,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // response bodies are simple structs/strings
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
