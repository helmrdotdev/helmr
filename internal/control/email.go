package control

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/resend/resend-go/v3"
)

const emailSMTPTimeout = 10 * time.Second
const emailHTTPTimeout = 10 * time.Second

type emailMessage struct {
	To             string
	Subject        string
	PlainText      string
	IdempotencyKey string
	MessageID      string

	magicLink *magicLinkMessage
}

type emailSender interface {
	SendEmail(ctx context.Context, message emailMessage) error
}

type unconfiguredEmailSender struct{}

func (unconfiguredEmailSender) SendEmail(context.Context, emailMessage) error {
	return errors.New("email sender is not configured")
}

type logEmailSender struct {
	log *slog.Logger
}

func (m logEmailSender) SendEmail(_ context.Context, message emailMessage) error {
	if message.magicLink != nil {
		m.log.Info("magic link email", "email", message.magicLink.Email, "purpose", message.magicLink.Purpose, "url", message.magicLink.URL, "expires_at", message.magicLink.ExpiresAt)
		return nil
	}
	m.log.Info("email notification", "to", message.To, "subject", message.Subject)
	return nil
}

type smtpEmailSender struct {
	addr     string
	username string
	password string
	from     string
}

func (m smtpEmailSender) SendEmail(ctx context.Context, message emailMessage) error {
	if strings.TrimSpace(m.addr) == "" || strings.TrimSpace(m.from) == "" {
		return errors.New("smtp email sender is not configured")
	}
	from, err := mail.ParseAddress(m.from)
	if err != nil {
		return fmt.Errorf("invalid email sender: %w", err)
	}
	to, err := mail.ParseAddress(message.To)
	if err != nil {
		return fmt.Errorf("invalid email recipient: %w", err)
	}
	host := m.addr
	if parsedHost, _, splitErr := strings.Cut(m.addr, ":"); splitErr {
		host = parsedHost
	}
	body := fmt.Sprintf(
		"Subject: %s\r\nFrom: %s\r\nTo: %s\r\nMessage-ID: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		normalizeEmailHeader(message.Subject),
		from.String(),
		to.String(),
		normalizeEmailHeader(message.MessageID),
		normalizeEmailBody(message.PlainText),
	)
	ctx, cancel := context.WithTimeout(ctx, emailSMTPTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: emailSMTPTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("smtp server does not support STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if m.username != "" || m.password != "" {
		if err := client.Auth(smtp.PlainAuth("", m.username, m.password, host)); err != nil {
			return err
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to.Address); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

type resendEmailService interface {
	SendWithOptions(ctx context.Context, params *resend.SendEmailRequest, options *resend.SendEmailOptions) (*resend.SendEmailResponse, error)
}

type resendEmailSender struct {
	from   string
	emails resendEmailService
}

func newResendEmailSender(apiKey string, from string) resendEmailSender {
	client := resend.NewCustomClient(&http.Client{Timeout: emailHTTPTimeout}, apiKey)
	return resendEmailSender{from: from, emails: client.Emails}
}

func (m resendEmailSender) SendEmail(ctx context.Context, message emailMessage) error {
	if strings.TrimSpace(m.from) == "" || m.emails == nil {
		return errors.New("resend email sender is not configured")
	}
	from, err := mail.ParseAddress(m.from)
	if err != nil {
		return fmt.Errorf("invalid email sender: %w", err)
	}
	to, err := mail.ParseAddress(message.To)
	if err != nil {
		return fmt.Errorf("invalid email recipient: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, emailHTTPTimeout)
	defer cancel()
	params := &resend.SendEmailRequest{
		From:    formatEmailAddress(*from),
		To:      []string{formatEmailAddress(*to)},
		Subject: normalizeEmailHeader(message.Subject),
		Text:    normalizeResendEmailBody(message.PlainText),
		Headers: map[string]string{
			"Message-ID": normalizeEmailHeader(message.MessageID),
		},
	}
	if params.Headers["Message-ID"] == "" {
		params.Headers = nil
	}
	options := &resend.SendEmailOptions{IdempotencyKey: normalizeEmailHeader(message.IdempotencyKey)}
	if options.IdempotencyKey == "" {
		options = nil
	}
	if _, err := m.emails.SendWithOptions(ctx, params, options); err != nil {
		return err
	}
	return nil
}

func formatEmailAddress(address mail.Address) string {
	if strings.TrimSpace(address.Name) == "" {
		return strings.TrimSpace(address.Address)
	}
	return address.String()
}

func normalizeEmailHeader(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func normalizeEmailBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return body
}

func normalizeResendEmailBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body
}

type magicLinkMailer interface {
	SendMagicLink(ctx context.Context, message magicLinkMessage) error
}

type legacyMagicLinkEmailSender struct {
	mailer magicLinkMailer
}

func (m legacyMagicLinkEmailSender) SendEmail(ctx context.Context, message emailMessage) error {
	if message.magicLink == nil {
		return errors.New("legacy magic link mailer cannot send generic email")
	}
	return m.mailer.SendMagicLink(ctx, *message.magicLink)
}
