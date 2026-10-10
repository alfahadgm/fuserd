package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// apiError is a failure reported to the client as {"error": {"code", "message"}}. The client matches code against
// the error literals it knows.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// Codes the client recognises. down_for_maintenance / maintenance are deliberately absent: either one flips the
// client to EHypeConnectionStatus::DownForMaintenance and disables the online UI.
const (
	codeUnknown    = "unknown_error"
	codeUserBanned = "user_banned"
)

func errBadRequest(msg string) *apiError { return &apiError{http.StatusBadRequest, codeUnknown, msg} }
func errUnauthorized(msg string) *apiError {
	return &apiError{http.StatusUnauthorized, codeUnknown, msg}
}
func errInternal() *apiError {
	return &apiError{http.StatusInternalServerError, codeUnknown, "internal error"}
}

// flatResults is a payload sent both as "results" and at the top level, for jobs that may not require the results
// field (the Main Stage polling URLs, mainstage.go). The client reads whichever its job expects and ignores the rest.
type flatResults map[string]any

// encodeResults builds the success envelope. The client rejects a response without a "results" field.
func encodeResults(v any) []byte {
	if v == nil {
		v = struct{}{}
	}
	if f, ok := v.(flatResults); ok {
		out := make(map[string]any, len(f)+1)
		for k, x := range f {
			out[k] = x
		}
		out["results"] = map[string]any(f)
		b, err := json.Marshal(out)
		if err != nil {
			return encodeError(errInternal())
		}
		return b
	}
	b, err := json.Marshal(struct {
		Results any `json:"results"`
	}{v})
	if err != nil {
		return encodeError(errInternal())
	}
	return b
}

func encodeError(e *apiError) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}})
	return b
}
