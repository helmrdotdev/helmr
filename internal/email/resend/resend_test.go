package resend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/email"
	resendapi "github.com/resend/resend-go/v3"
)

func TestResendEmailSenderSendsPlainTextEmail(t *testing.T) {
	service := &recordingResendEmailService{}
	sender := Sender{from: "Helmr <noreply@example.test>", emails: service}

	err := sender.SendEmail(context.Background(), email.Message{
		To:             "Owner <owner@example.test>",
		Subject:        "Hello\nWorld",
		PlainText:      "line one\r\nline two",
		IdempotencyKey: "notification-delivery/123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.request == nil {
		t.Fatal("request was not sent")
	}
	if service.request.From != `"Helmr" <noreply@example.test>` || strings.Join(service.request.To, ",") != `"Owner" <owner@example.test>` {
		t.Fatalf("request recipients = from %q to %+v", service.request.From, service.request.To)
	}
	if service.request.Subject != "Hello World" {
		t.Fatalf("subject = %q", service.request.Subject)
	}
	if service.request.Text != "line one\nline two\n" {
		t.Fatalf("text = %q", service.request.Text)
	}
	if service.options == nil || service.options.IdempotencyKey != "notification-delivery/123" {
		t.Fatalf("options = %+v", service.options)
	}
}

func TestResendEmailSenderSendsBareRecipientAddressWithoutAngleBrackets(t *testing.T) {
	service := &recordingResendEmailService{}
	sender := Sender{from: "noreply@example.test", emails: service}

	if err := sender.SendEmail(context.Background(), email.Message{To: "owner@example.test", Subject: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if service.request.From != "noreply@example.test" || strings.Join(service.request.To, ",") != "owner@example.test" {
		t.Fatalf("request recipients = from %q to %+v", service.request.From, service.request.To)
	}
}

func TestResendEmailSenderRejectsInvalidAddresses(t *testing.T) {
	sender := Sender{from: "noreply@example.test", emails: &recordingResendEmailService{}}
	if err := sender.SendEmail(context.Background(), email.Message{To: "bad address", Subject: "Hello"}); err == nil {
		t.Fatal("expected invalid recipient error")
	}
	sender.from = "bad address"
	if err := sender.SendEmail(context.Background(), email.Message{To: "owner@example.test", Subject: "Hello"}); err == nil {
		t.Fatal("expected invalid sender error")
	}
}

func TestResendEmailSenderPropagatesSendError(t *testing.T) {
	sender := Sender{
		from:   "noreply@example.test",
		emails: &recordingResendEmailService{err: errors.New("resend failed")},
	}
	if err := sender.SendEmail(context.Background(), email.Message{To: "owner@example.test", Subject: "Hello"}); err == nil || !strings.Contains(err.Error(), "resend failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestResendEmailSenderPropagatesCancellation(t *testing.T) {
	service := &recordingResendEmailService{waitForCancellation: true}
	sender := Sender{from: "noreply@example.test", emails: service}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sender.SendEmail(ctx, email.Message{To: "owner@example.test", Subject: "Hello"})
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SendEmail error = %v, want cancellation", err)
	}
}

type recordingResendEmailService struct {
	request             *resendapi.SendEmailRequest
	options             *resendapi.SendEmailOptions
	err                 error
	waitForCancellation bool
}

func (s *recordingResendEmailService) SendWithOptions(ctx context.Context, params *resendapi.SendEmailRequest, options *resendapi.SendEmailOptions) (*resendapi.SendEmailResponse, error) {
	s.request = params
	s.options = options
	if s.waitForCancellation {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return &resendapi.SendEmailResponse{Id: "email-id"}, nil
}
