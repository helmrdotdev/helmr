package control

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type errKind int

const (
	errBadRequest errKind = iota + 1
	errUnauthorized
	errForbidden
	errNotFound
	errConflict
	errTooLarge
	errBadGateway
	errUnavailable
)

var errRecordNotFound = errors.New("record not found")

type apiError struct {
	kind errKind
	err  error
}

func (e apiError) Error() string {
	return e.err.Error()
}

func (e apiError) Unwrap() error {
	return e.err
}

func badRequest(err error) error {
	return apiError{kind: errBadRequest, err: err}
}

func unauthorized(err error) error {
	return apiError{kind: errUnauthorized, err: err}
}

func forbidden(err error) error {
	return apiError{kind: errForbidden, err: err}
}

func notFound(err error) error {
	return apiError{kind: errNotFound, err: err}
}

func conflict(err error) error {
	return apiError{kind: errConflict, err: err}
}

func tooLarge(err error) error {
	return apiError{kind: errTooLarge, err: err}
}

func badGateway(err error) error {
	return apiError{kind: errBadGateway, err: err}
}

func unavailable(err error) error {
	return apiError{kind: errUnavailable, err: err}
}

func errorStatus(err error) int {
	var apiErr apiError
	if !errors.As(err, &apiErr) {
		return http.StatusInternalServerError
	}
	switch apiErr.kind {
	case errBadRequest:
		return http.StatusBadRequest
	case errUnauthorized:
		return http.StatusUnauthorized
	case errForbidden:
		return http.StatusForbidden
	case errNotFound:
		return http.StatusNotFound
	case errConflict:
		return http.StatusConflict
	case errTooLarge:
		return http.StatusRequestEntityTooLarge
	case errBadGateway:
		return http.StatusBadGateway
	case errUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	writeErrorStatus(w, errorStatus(err), err)
}

func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errRecordNotFound)
}
