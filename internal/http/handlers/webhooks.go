package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/softsrv/rowbot/internal/concept2"
)

// rowingServicer is the narrow interface the webhook handler needs to process
// an incoming Concept2 result asynchronously.
type rowingServicer interface {
	ProcessResult(ctx context.Context, concept2UserID int64, resultID int64) error
}

// WebhookHandler handles inbound webhook deliveries from external services.
type WebhookHandler struct {
	svc rowingServicer
}

// NewWebhookHandler constructs a WebhookHandler. svc may be nil; when nil,
// results are logged but not processed.
func NewWebhookHandler(svc rowingServicer) *WebhookHandler {
	return &WebhookHandler{svc: svc}
}

// maxLoggedBodyBytes caps how much of a raw webhook body gets echoed into
// logs, so a malformed-but-huge payload can't blow up log volume.
const maxLoggedBodyBytes = 2000

// truncatedBody returns body as a string for logging, capped at
// maxLoggedBodyBytes with a marker appended if it was cut off.
func truncatedBody(body []byte) string {
	if len(body) <= maxLoggedBodyBytes {
		return string(body)
	}
	return string(body[:maxLoggedBodyBytes]) + "…(truncated)"
}

// eventResultID returns the result ID to log for an ignored event, preferring
// the nested Result.ID (used by result-added/result-updated) and falling
// back to the sibling ResultID (used by result-deleted) when no result
// object is present.
func eventResultID(payload concept2.Concept2Payload) int64 {
	if payload.Result != nil {
		return payload.Result.ID
	}
	return payload.ResultID
}

// Concept2 handles POST /webhooks/concept2.
//
// Concept2 does not sign or otherwise authenticate its webhook deliveries, so
// trust is established structurally rather than cryptographically: the body
// must parse into the expected shape, and the delivered result_id/user_id are
// only ever used to trigger a fetch of the real result from the Concept2 API
// using our own stored OAuth token (see RowingService.ProcessResult) — the
// webhook body itself is never trusted as a source of result data.
//
// Every rejection path logs at Warn (visible in prod, not just dev) with
// enough context — including the raw body where relevant — to tell exactly
// where and why a delivery was rejected without needing to reproduce it.
//
// Validation order (failing closed at the first problem):
//  1. Content-Type must contain "application/json" — 415 otherwise.
//  2. Raw body read — 400 on error.
//  3. JSON unmarshal — 400 on error.
//  4. Required field validation (type must be non-empty) — 400 otherwise.
//  5. Event type filter — only "result-added" is processed; other known event
//     types ("result-updated", "result-deleted") are logged and acknowledged
//     with 200 without further action.
//  6. Log, kick off async processing, and return 200.
func (h *WebhookHandler) Concept2(w http.ResponseWriter, r *http.Request) {
	slog.Debug("concept2 webhook: request received",
		"content_type", r.Header.Get("Content-Type"),
		"content_length", r.ContentLength,
	)

	// 1. Content-Type check.
	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		slog.Warn("concept2 webhook: rejected — unsupported content type",
			"content_type", contentType,
		)
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	// 2. Read raw body. BodyLimit middleware has already capped it at 1 MiB.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Warn("concept2 webhook: rejected — failed to read body",
			"error", err,
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	slog.Debug("concept2 webhook: raw body", "body", truncatedBody(body))

	// 3. JSON decode.
	var payload concept2.Concept2Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("concept2 webhook: rejected — invalid JSON",
			"error", err,
			"body", truncatedBody(body),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 4. Validate required fields.
	if payload.Type == "" {
		slog.Warn("concept2 webhook: rejected — empty/missing \"type\" field after decode; "+
			"this usually means the payload's JSON shape doesn't match what we expect "+
			"(see concept2.Concept2Payload) — check the logged body against it",
			"body", truncatedBody(body),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 5. Only "result-added" events are processed; other event types
	// (e.g. "result-updated", "result-deleted") are legitimate but not yet
	// acted on, so acknowledge with 200 to avoid triggering Concept2 retries.
	if payload.Type != "result-added" {
		slog.Info("concept2 webhook: ignoring event type",
			"event_type", payload.Type,
			"result_id", eventResultID(payload),
		)
		w.WriteHeader(http.StatusOK)
		return
	}

	// A "result-added" event without a nested result object is malformed —
	// reject rather than risk a nil-pointer dereference below.
	if payload.Result == nil {
		slog.Warn("concept2 webhook: rejected — \"result-added\" event has no nested \"result\" object; "+
			"check the logged body against concept2.Concept2Payload",
			"body", truncatedBody(body),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	concept2UserID := payload.Result.UserID
	resultID := payload.Result.ID

	// 6. Log and process synchronously — Concept2's webhook delivery timeout
	// isn't documented anywhere, so responding only once processing is done
	// (rather than acking immediately and continuing in a goroutine) avoids
	// relying on Cloud Run's default CPU throttling being loosened for
	// post-response background work to actually run.
	slog.Info("concept2 webhook received",
		"event_type", payload.Type,
		"concept2_user_id", concept2UserID,
		"result_id", resultID,
	)

	switch {
	case h.svc == nil:
		slog.Warn("concept2 webhook: not processing — no rowing service configured",
			"result_id", resultID,
		)
	case resultID == 0:
		slog.Warn("concept2 webhook: not processing — result id is 0/missing after decode; "+
			"check the logged body above against concept2.Concept2Payload's json tags",
			"concept2_user_id", concept2UserID,
		)
	default:
		// Derived from the request context (rather than context.Background())
		// so that Concept2 disconnecting early cancels our work too, capped at
		// 3 minutes as a safety upper bound in case the request context has no
		// deadline of its own.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		slog.Info("concept2 webhook: processing started",
			"concept2_user_id", concept2UserID,
			"result_id", resultID,
		)
		if err := h.svc.ProcessResult(ctx, concept2UserID, resultID); err != nil {
			slog.Error("concept2 webhook: process result failed",
				"result_id", resultID,
				"error", err,
			)
		} else {
			slog.Info("concept2 webhook: processing completed",
				"concept2_user_id", concept2UserID,
				"result_id", resultID,
			)
		}
	}

	w.WriteHeader(http.StatusOK)
}
