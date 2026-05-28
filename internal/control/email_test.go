package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/resend/resend-go/v3"
)

func TestResendEmailSenderSendsPlainTextEmail(t *testing.T) {
	service := &recordingResendEmailService{}
	sender := resendEmailSender{from: "Helmr <noreply@example.test>", emails: service}

	err := sender.SendEmail(context.Background(), emailMessage{
		To:             "Owner <owner@example.test>",
		Subject:        "Hello\nWorld",
		PlainText:      "line one\r\nline two",
		IdempotencyKey: "waitpoint-delivery/123",
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
	if service.options == nil || service.options.IdempotencyKey != "waitpoint-delivery/123" {
		t.Fatalf("options = %+v", service.options)
	}
}

func TestResendEmailSenderSendsBareRecipientAddressWithoutAngleBrackets(t *testing.T) {
	service := &recordingResendEmailService{}
	sender := resendEmailSender{from: "noreply@example.test", emails: service}

	if err := sender.SendEmail(context.Background(), emailMessage{To: "owner@example.test", Subject: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if service.request.From != "noreply@example.test" || strings.Join(service.request.To, ",") != "owner@example.test" {
		t.Fatalf("request recipients = from %q to %+v", service.request.From, service.request.To)
	}
}

func TestResendEmailSenderRejectsInvalidAddresses(t *testing.T) {
	sender := resendEmailSender{from: "noreply@example.test", emails: &recordingResendEmailService{}}
	if err := sender.SendEmail(context.Background(), emailMessage{To: "bad address", Subject: "Hello"}); err == nil {
		t.Fatal("expected invalid recipient error")
	}
	sender.from = "bad address"
	if err := sender.SendEmail(context.Background(), emailMessage{To: "owner@example.test", Subject: "Hello"}); err == nil {
		t.Fatal("expected invalid sender error")
	}
}

func TestResendEmailSenderPropagatesSendError(t *testing.T) {
	sender := resendEmailSender{
		from:   "noreply@example.test",
		emails: &recordingResendEmailService{err: errors.New("resend failed")},
	}
	if err := sender.SendEmail(context.Background(), emailMessage{To: "owner@example.test", Subject: "Hello"}); err == nil || !strings.Contains(err.Error(), "resend failed") {
		t.Fatalf("error = %v", err)
	}
}

type recordingResendEmailService struct {
	request *resend.SendEmailRequest
	options *resend.SendEmailOptions
	err     error
}

func (s *recordingResendEmailService) SendWithOptions(_ context.Context, params *resend.SendEmailRequest, options *resend.SendEmailOptions) (*resend.SendEmailResponse, error) {
	s.request = params
	s.options = options
	if s.err != nil {
		return nil, s.err
	}
	return &resend.SendEmailResponse{Id: "email-id"}, nil
}
