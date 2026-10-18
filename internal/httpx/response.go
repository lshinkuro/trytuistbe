// Package httpx berisi helper HTTP bersama: envelope response, error, decode
// request, paginasi, dan middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Meta untuk endpoint list berpaginasi.
type Meta struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

type envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	Meta    *Meta  `json:"meta,omitempty"`
}

type errorBody struct {
	Type    string        `json:"type"`
	Details []FieldDetail `json:"details,omitempty"`
}

type errorEnvelope struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Error   errorBody `json:"error"`
}

// JSON menulis envelope sukses: { code, message: "success", data, meta? }.
func JSON(w http.ResponseWriter, status int, data any, meta ...Meta) {
	JSONMessage(w, status, "success", data, meta...)
}

// JSONMessage sama dengan JSON tapi dengan pesan khusus (mis. "Pesanan diterima").
func JSONMessage(w http.ResponseWriter, status int, message string, data any, meta ...Meta) {
	env := envelope{Code: status, Message: message, Data: data}
	if len(meta) > 0 {
		m := meta[0]
		env.Meta = &m
	}
	write(w, status, env)
}

// Error menulis envelope error.
func Error(w http.ResponseWriter, status int, errType, message string, details ...FieldDetail) {
	write(w, status, errorEnvelope{
		Code:    status,
		Message: message,
		Error:   errorBody{Type: errType, Details: details},
	})
}

// FromError memetakan error dari service ke response. *AppError dipakai apa
// adanya; error lain dicatat dan dikembalikan sebagai 500.
func FromError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		Error(w, appErr.Status, appErr.Type, appErr.Message, appErr.Details...)
		return
	}
	slog.ErrorContext(r.Context(), "internal error", "err", err, "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
	Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan pada server. Coba lagi nanti.")
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode response", "err", err)
	}
}

// Loc adalah zona waktu untuk semua timestamp di response (di-set dari config).
var Loc = time.FixedZone("WIB", 7*3600)

// ISO memformat waktu sebagai ISO-8601 dengan zona waktu, mis. 2026-10-17T14:30:00+07:00.
func ISO(t time.Time) string { return t.In(Loc).Format(time.RFC3339) }

// ISOPtr seperti ISO tapi menghasilkan null untuk waktu kosong.
func ISOPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ISO(*t)
	return &s
}
