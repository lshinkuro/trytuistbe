package httpx

import (
	"fmt"
	"net/http"
)

// AppError adalah error yang sudah tahu bentuk response-nya.
type AppError struct {
	Status  int
	Type    string
	Message string
	Details []FieldDetail
}

func (e *AppError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Type, e.Message) }

// FieldDetail menjelaskan field yang gagal validasi.
type FieldDetail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func NewError(status int, errType, message string) *AppError {
	return &AppError{Status: status, Type: errType, Message: message}
}

// Konstruktor untuk error yang sering dipakai (lihat API_PLAN.md §3.3).

func Validation(message string, details ...FieldDetail) *AppError {
	return &AppError{Status: http.StatusBadRequest, Type: "VALIDATION_ERROR", Message: message, Details: details}
}

func Unauthorized(message string) *AppError {
	return NewError(http.StatusUnauthorized, "UNAUTHORIZED", message)
}

func NotFound(message string) *AppError {
	return NewError(http.StatusNotFound, "NOT_FOUND", message)
}

func Conflict(errType, message string) *AppError {
	return NewError(http.StatusConflict, errType, message)
}

func Unprocessable(errType, message string) *AppError {
	return NewError(http.StatusUnprocessableEntity, errType, message)
}
