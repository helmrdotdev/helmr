package resend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/email"
	resendapi "github.com/resend/resend-go/v3"
)

const httpTimeout = 10 * time.Second

type emailService interface {
	SendWithOptions(ctx context.Context, params *resendapi.SendEmailRequest, options *resendapi.SendEmailOptions) (*resendapi.SendEmailResponse, error)
}

type Sender struct {
	from   string
	emails emailService
}

func New(apiKey string, from string) Sender {
	client := resendapi.NewCustomClient(&http.Client{Timeout: httpTimeout}, apiKey)
	return Sender{from: from, emails: client.Emails}
}

func (sender Sender) SendEmail(ctx context.Context, message email.Message) error {
	if strings.TrimSpace(sender.from) == "" || sender.emails == nil {
		return errors.New("resend email sender is not configured")
	}
	from, err := mail.ParseAddress(sender.from)
	if err != nil {
		return fmt.Errorf("invalid email sender: %w", err)
	}
	to, err := mail.ParseAddress(message.To)
	if err != nil {
		return fmt.Errorf("invalid email recipient: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	params := &resendapi.SendEmailRequest{
		From:    email.FormatAddress(*from),
		To:      []string{email.FormatAddress(*to)},
		Subject: email.NormalizeHeader(message.Subject),
		Text:    normalizeBody(message.PlainText),
		Headers: map[string]string{
			"Message-ID": email.NormalizeHeader(message.MessageID),
		},
	}
	if params.Headers["Message-ID"] == "" {
		params.Headers = nil
	}
	options := &resendapi.SendEmailOptions{IdempotencyKey: email.NormalizeHeader(message.IdempotencyKey)}
	if options.IdempotencyKey == "" {
		options = nil
	}
	if _, err := sender.emails.SendWithOptions(ctx, params, options); err != nil {
		return err
	}
	return nil
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body
}
