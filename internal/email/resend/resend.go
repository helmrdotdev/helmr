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
	resendapi "github.com/resend/resend-go/v4"
)

const httpTimeout = 10 * time.Second

type emailService interface {
	SendWithOptions(ctx context.Context, params *resendapi.SendEmailRequest, options *resendapi.SendEmailOptions) (*resendapi.SendEmailResponse, error)
}

type batchService interface {
	SendWithOptions(ctx context.Context, params []*resendapi.SendEmailRequest, options *resendapi.BatchSendEmailOptions) (*resendapi.BatchEmailResponse, error)
}

// maxBatchEmails is the most emails one batch request may carry.
const maxBatchEmails = 100

type Sender struct {
	from   string
	emails emailService
	batch  batchService
}

func New(apiKey string, from string) Sender {
	client := resendapi.NewCustomClient(&http.Client{Timeout: httpTimeout}, apiKey)
	return Sender{from: from, emails: client.Emails, batch: client.Batch}
}

func (sender Sender) SendEmail(ctx context.Context, message email.Message) error {
	if strings.TrimSpace(sender.from) == "" || sender.emails == nil {
		return errors.New("resend email sender is not configured")
	}
	params, err := sender.request(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	options := &resendapi.SendEmailOptions{IdempotencyKey: email.NormalizeHeader(message.IdempotencyKey)}
	if options.IdempotencyKey == "" {
		options = nil
	}
	if _, err := sender.emails.SendWithOptions(ctx, params, options); err != nil {
		return err
	}
	return nil
}

// SendEmails sends the messages through the batch endpoint, up to 100 per
// request. Each request is validated strictly, so it sends all of its emails
// or none.
func (sender Sender) SendEmails(ctx context.Context, messages []email.Message) error {
	if strings.TrimSpace(sender.from) == "" || sender.batch == nil {
		return errors.New("resend email sender is not configured")
	}
	params := make([]*resendapi.SendEmailRequest, len(messages))
	for i, message := range messages {
		request, err := sender.request(message)
		if err != nil {
			return err
		}
		params[i] = request
	}
	for start := 0; start < len(messages); start += maxBatchEmails {
		if err := sender.sendBatch(ctx, params[start:min(start+maxBatchEmails, len(messages))]); err != nil {
			return err
		}
	}
	return nil
}

func (sender Sender) sendBatch(ctx context.Context, params []*resendapi.SendEmailRequest) error {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	_, err := sender.batch.SendWithOptions(ctx, params, &resendapi.BatchSendEmailOptions{BatchValidation: resendapi.BatchValidationStrict})
	return err
}

func (sender Sender) request(message email.Message) (*resendapi.SendEmailRequest, error) {
	from, err := mail.ParseAddress(sender.from)
	if err != nil {
		return nil, fmt.Errorf("invalid email sender: %w", err)
	}
	to, err := mail.ParseAddress(message.To)
	if err != nil {
		return nil, fmt.Errorf("invalid email recipient: %w", err)
	}
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
	return params, nil
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body
}
