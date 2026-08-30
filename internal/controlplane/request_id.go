package controlplane

import (
	"net/http"
	"uuid"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const requestIDHeader = "X-Request-ID"

func (s *Server) requestCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.NewV7()
		value := requestID.String()
		w.Header().Set(requestIDHeader, value)
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("helmr.request_id", value))
		next.ServeHTTP(w, r)
	})
}
