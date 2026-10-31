package httpapi

import (
	"encoding/json"
	"net/http"
)

// Code is the spec's Error.error.code.
type Code string

const (
	CodeInvalidRequest Code = "invalid_request"
	CodeUnauthorized   Code = "unauthorized"
)

type errorBody struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Code    Code          `json:"code"`
	Message string        `json:"message"`
	Details *errorDetails `json:"details,omitempty"`
}

type errorDetails struct {
	Field string `json:"field,omitempty"`
}

// WriteError writes the spec's Error. message is for developers, in English, and must not
// contain a secret. field, if not empty, is details.field.
func WriteError(w http.ResponseWriter, status int, code Code, message, field string) {
	body := errorBody{Error: errorObject{Code: code, Message: message}}
	if field != "" {
		body.Error.Details = &errorDetails{Field: field}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
