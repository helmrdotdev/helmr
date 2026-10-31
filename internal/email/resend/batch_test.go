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
// failAt (1-based) when it is positive and rejecting each email addressed to
// reject.
func newBatchServer(t *testing.T, failAt int, reject string) (Sender, *[]batchRequest) {
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
		data := []map[string]string{}
		errs := []map[string]any{}
		for i, sent := range emails {
			if reject != "" && strings.Join(sent.To, ",") == reject {
				errs = append(errs, map[string]any{"index": i, "message": "invalid recipient"})
				continue
			}
			data = append(data, map[string]string{"id": fmt.Sprintf("email-%d-%d", number, i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
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
	sender, requests := newBatchServer(t, 0, "")
	messages := batchMessages(205)
	for i, err := range sender.SendEmails(t.Context(), messages) {
		if err != nil {
			t.Fatalf("email %d: %v", i, err)
		}
	}
	sizes := []int{100, 100, 5}
	if len(*requests) != len(sizes) {
		t.Fatalf("requests = %d, want %d", len(*requests), len(sizes))
	}
	next := 0
	for i, request := range *requests {
		if len(request.emails) != sizes[i] || request.validation != "permissive" {
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

func TestResendSenderFailsOnlyTheEmailsOfAFailedBatch(t *testing.T) {
	sender, requests := newBatchServer(t, 2, "")
	errs := sender.SendEmails(t.Context(), batchMessages(250))
	if len(*requests) != 3 {
		t.Fatalf("requests = %d, want every batch attempted", len(*requests))
	}
	for i, err := range errs {
		if failed := i >= 100 && i < 200; (err != nil) != failed {
			t.Fatalf("email %d error = %v, want failed=%v", i, err, failed)
		}
	}
}

func TestResendSenderFailsOnlyTheRejectedEmail(t *testing.T) {
	sender, _ := newBatchServer(t, 0, "person-1@example.test")
	errs := sender.SendEmails(t.Context(), batchMessages(3))
	if len(errs) != 3 || errs[0] != nil || errs[1] == nil || errs[2] != nil {
		t.Fatalf("errors = %v, want only the rejected email to fail", errs)
	}
}

func TestResendSenderFailsAnInvalidRecipientWithoutSendingIt(t *testing.T) {
	sender, requests := newBatchServer(t, 0, "")
	messages := batchMessages(2)
	messages[1].To = "not an address"
	errs := sender.SendEmails(t.Context(), messages)
	if len(errs) != 2 || errs[0] != nil || errs[1] == nil {
		t.Fatalf("errors = %v, want only the invalid recipient to fail", errs)
	}
	if len(*requests) != 1 || len((*requests)[0].emails) != 1 {
		t.Fatalf("requests = %+v, want one request with the valid email", *requests)
	}
}
