package resend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/email"
	resendapi "github.com/resend/resend-go/v4"
)

type batchRequest struct {
	validation string
	emails     []resendapi.SendEmailRequest
}

// newBatchServer serves the batch endpoint, failing the request numbered
// failAt (1-based) when it is positive.
func newBatchServer(t *testing.T, failAt int) (Sender, *[]batchRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []batchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails/batch" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		var emails []resendapi.SendEmailRequest
		if err := json.NewDecoder(r.Body).Decode(&emails); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, batchRequest{validation: r.Header.Get("x-batch-validation"), emails: emails})
		number := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if number == failAt {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"statusCode":422,"name":"validation_error","message":"rejected"}`))
			return
		}
		data := make([]map[string]string, len(emails))
		for i := range emails {
			data[i] = map[string]string{"id": fmt.Sprintf("email-%d-%d", number, i)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	client := resendapi.NewCustomClient(server.Client(), "re_test")
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = baseURL
	return Sender{from: "Helmr <noreply@example.test>", batch: client.Batch}, &requests
}

func batchMessages(count int) []email.Message {
	messages := make([]email.Message, count)
	for i := range messages {
		messages[i] = email.Message{To: fmt.Sprintf("person-%d@example.test", i), Subject: fmt.Sprintf("Hello %d", i), PlainText: fmt.Sprintf("body %d", i)}
	}
	return messages
}

func TestResendSenderSendsBatchesOfAtMostOneHundred(t *testing.T) {
	sender, requests := newBatchServer(t, 0)
	messages := batchMessages(205)
	if err := sender.SendEmails(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	sizes := []int{100, 100, 5}
	if len(*requests) != len(sizes) {
		t.Fatalf("requests = %d, want %d", len(*requests), len(sizes))
	}
	next := 0
	for i, request := range *requests {
		if len(request.emails) != sizes[i] || request.validation != "strict" {
			t.Fatalf("request %d has %d emails, validation %q", i, len(request.emails), request.validation)
		}
		for _, sent := range request.emails {
			want := messages[next]
			if sent.From != `"Helmr" <noreply@example.test>` || strings.Join(sent.To, ",") != want.To || sent.Subject != want.Subject || sent.Text != want.PlainText+"\n" {
				t.Fatalf("email %d = %+v", next, sent)
			}
			next++
		}
	}
}

func TestResendSenderStopsAtTheFirstFailedBatch(t *testing.T) {
	sender, requests := newBatchServer(t, 2)
	if err := sender.SendEmails(t.Context(), batchMessages(250)); err == nil {
		t.Fatal("expected the failed batch to fail the send")
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %d, want the send to stop at the failed batch", len(*requests))
	}
}

func TestResendSenderRejectsAnInvalidRecipientBeforeSending(t *testing.T) {
	sender, requests := newBatchServer(t, 0)
	messages := batchMessages(2)
	messages[1].To = "not an address"
	if err := sender.SendEmails(t.Context(), messages); err == nil {
		t.Fatal("expected invalid recipient error")
	}
	if len(*requests) != 0 {
		t.Fatalf("requests = %d, want none", len(*requests))
	}
}
