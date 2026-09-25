package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputerSaveHandlersRejectMalformedRequestsBeforeStorage(t *testing.T) {
	s := &Server{}
	handlers := map[string]http.HandlerFunc{
		"begin": s.workerBeginComputerSave, "abandon": s.workerAbandonComputerSave,
		"publish": s.workerPublishComputerSave, "adopt": s.workerAdoptComputerSave,
		"register": s.workerRegisterComputerSaveObject, "certify": s.workerCertifyComputerSaveObject, "reuse": s.workerReuseComputerSaveObject,
	}
	for name, handler := range handlers {
		for _, body := range []string{`{}`, `{"unknown":true}`, `{} {}`, `null`} {
			t.Run(name+"/"+body, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				handler(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
}
