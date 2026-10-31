package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

const emailSMTPTimeout = 10 * time.Second

type Message struct {
	To             string
	Subject        string
	PlainText      string
	IdempotencyKey string
	MessageID      string

	// Link is the single-use link the message delivers, which LogSender logs
	// for local use.
	Link *Link
}

// Link is a single-use link sent by email, such as a sign-in or invitation
// link.
type Link struct {
	Email     string
	Purpose   string
	URL       string
	ExpiresAt time.Time
}

// Sender delivers email. SendEmails sends several messages with the fewest
// provider calls and returns one error per message, in order: nil when that
// message was sent. One message that cannot be sent does not stop the others.
type Sender interface {
	SendEmail(ctx context.Context, message Message) error
	SendEmails(ctx context.Context, messages []Message) []error
}

// FailAll reports err for each of count messages.
func FailAll(count int, err error) []error {
	errs := make([]error, count)
	for i := range errs {
		errs[i] = err
	}
	return errs
}

type Unconfigured struct{}

func (Unconfigured) SendEmail(context.Context, Message) error {
	return errors.New("email sender is not configured")
}

func (Unconfigured) SendEmails(_ context.Context, messages []Message) []error {
	return FailAll(len(messages), errors.New("email sender is not configured"))
}

type LogSender struct {
	Log *slog.Logger
}

func (m LogSender) SendEmail(_ context.Context, message Message) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	if message.Link != nil {
		log.Info("link email", "email", message.Link.Email, "purpose", message.Link.Purpose, "url", message.Link.URL, "expires_at", message.Link.ExpiresAt)
		return nil
	}
	log.Info("email notification", "to", message.To, "subject", message.Subject)
	return nil
}

func (m LogSender) SendEmails(ctx context.Context, messages []Message) []error {
	errs := make([]error, len(messages))
	for i, message := range messages {
		errs[i] = m.SendEmail(ctx, message)
	}
	return errs
}

type SMTPSender struct {
	addr     string
	username string
	password string
	from     string
	// rootCAs verifies the server certificate; nil uses the system roots.
	rootCAs *x509.CertPool
}

func NewSMTPSender(addr string, username string, password string, from string) SMTPSender {
	return SMTPSender{
		addr:     addr,
		username: username,
		password: password,
		from:     from,
	}
}

func (m SMTPSender) SendEmail(ctx context.Context, message Message) error {
	return m.SendEmails(ctx, []Message{message})[0]
}

// SendEmails sends every message over one SMTP session, resetting the
// transaction between messages. A message the server refuses fails on its own
// and the session continues with the next one; a failure of the session itself
// fails every message not yet sent. A failed QUIT after every message was
// handled is logged, not reported.
func (m SMTPSender) SendEmails(ctx context.Context, messages []Message) []error {
	errs := make([]error, len(messages))
	if strings.TrimSpace(m.addr) == "" || strings.TrimSpace(m.from) == "" {
		return FailAll(len(messages), errors.New("smtp email sender is not configured"))
	}
	from, err := mail.ParseAddress(m.from)
	if err != nil {
		return FailAll(len(messages), fmt.Errorf("invalid email sender: %w", err))
	}
	recipients := make([]*mail.Address, len(messages))
	pending := 0
	for i, message := range messages {
		if recipients[i], err = mail.ParseAddress(message.To); err != nil {
			errs[i] = fmt.Errorf("invalid email recipient: %w", err)
			continue
		}
		pending++
	}
	if pending == 0 {
		return errs
	}
	// failRest fails every message that has not been handled yet.
	failRest := func(from int, err error) []error {
		for i := from; i < len(messages); i++ {
			if recipients[i] != nil && errs[i] == nil {
				errs[i] = err
			}
		}
		return errs
	}
	host := m.addr
	if parsedHost, _, splitErr := strings.Cut(m.addr, ":"); splitErr {
		host = parsedHost
	}
	// The session gets the per-message timeout once for connecting and once
	// for each message.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(pending+1)*emailSMTPTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: emailSMTPTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return failRest(0, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return failRest(0, err)
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return failRest(0, errors.New("smtp server does not support STARTTLS"))
	}
	if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: m.rootCAs}); err != nil {
		return failRest(0, err)
	}
	if m.username != "" || m.password != "" {
		if err := client.Auth(smtp.PlainAuth("", m.username, m.password, host)); err != nil {
			return failRest(0, err)
		}
	}
	started := false
	for i, message := range messages {
		if recipients[i] == nil {
			continue
		}
		if started {
			if err := client.Reset(); err != nil {
				return failRest(i, err)
			}
		}
		started = true
		if err := sendSMTPMessage(client, from, recipients[i], message); err != nil {
			errs[i] = fmt.Errorf("send email to %s: %w", recipients[i].Address, err)
		}
	}
	if err := client.Quit(); err != nil {
		slog.Default().Warn("smtp quit failed after handling every message", "error", err)
	}
	return errs
}

func sendSMTPMessage(client *smtp.Client, from *mail.Address, to *mail.Address, message Message) error {
	headers := []string{
		"Subject: " + mime.QEncoding.Encode("utf-8", NormalizeHeader(message.Subject)),
		"From: " + from.String(),
		"To: " + to.String(),
	}
	if messageID := NormalizeHeader(message.MessageID); messageID != "" {
		headers = append(headers, "Message-ID: "+messageID)
	}
	headers = append(headers, "Content-Type: text/plain; charset=utf-8")
	body := fmt.Sprintf(
		"%s\r\n\r\n%s",
		strings.Join(headers, "\r\n"),
		normalizeEmailBody(message.PlainText),
	)
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
	return writer.Close()
}

func FormatAddress(address mail.Address) string {
	if strings.TrimSpace(address.Name) == "" {
		return strings.TrimSpace(address.Address)
	}
	return address.String()
}

func NormalizeHeader(value string) string {
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
