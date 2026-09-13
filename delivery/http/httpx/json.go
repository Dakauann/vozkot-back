package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxBodySize = 1 << 20

type ErrorResponse struct {
	Error string `json:"error"`
	// Code names the failure in a form a client can branch on. Absent when
	// there is no useful distinction to draw.
	//
	// It exists because `Error` is written for a person, in one language: a
	// client that matches on it breaks the first time the wording improves or a
	// second locale is added. A UI that has to offer a different way out for
	// "you are already holding too many" than for "these sold out" needs
	// something stable to key on, and this is it.
	Code string `json:"code,omitempty"`
}

func ReadJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxBodySize)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func WriteJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func WriteError(response http.ResponseWriter, status int, err error) {
	WriteJSON(response, status, ErrorResponse{Error: err.Error()})
}

// WriteCodedError is WriteError with the machine-readable half filled in.
func WriteCodedError(response http.ResponseWriter, status int, code string, err error) {
	WriteJSON(response, status, ErrorResponse{Error: err.Error(), Code: code})
}
