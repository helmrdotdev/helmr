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

// Sender delivers email. SendEmails sends several messages as one batch with
// the fewest provider calls; it reports an error when any message of the
// batch may not have been sent. A provider that delivers message by message
// (SMTP) may already have sent earlier messages of a batch that fails.
type Sender interface {
	SendEmail(ctx context.Context, message Message) error
	SendEmails(ctx context.Context, messages []Message) error
}

type Unconfigured struct{}

func (Unconfigured) SendEmail(context.Context, Message) error {
	return errors.New("email sender is not configured")
}

func (Unconfigured) SendEmails(context.Context, []Message) error {
	return errors.New("email sender is not configured")
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

func (m LogSender) SendEmails(ctx context.Context, messages []Message) error {
	for _, message := range messages {
		if err := m.SendEmail(ctx, message); err != nil {
			return err
		}
	}
	return nil
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
	return m.SendEmails(ctx, []Message{message})
}

// RecipientError reports the recipient whose message the SMTP server refused.
// Messages before it in the batch were already sent.
type RecipientError struct {
	Address string
	Err     error
}

func (e RecipientError) Error() string {
	return fmt.Sprintf("send email to %s: %v", e.Address, e.Err)
}

func (e RecipientError) Unwrap() error {
	return e.Err
}

// SendEmails sends every message over one SMTP session, resetting the
// transaction between messages. SMTP delivers message by message: when the
// server refuses a message, the earlier ones were already sent, and the error
// is a RecipientError naming the refused recipient. A failed QUIT after every
// message was accepted is logged, not reported.
func (m SMTPSender) SendEmails(ctx context.Context, messages []Message) error {
	if strings.TrimSpace(m.addr) == "" || strings.TrimSpace(m.from) == "" {
		return errors.New("smtp email sender is not configured")
	}
	from, err := mail.ParseAddress(m.from)
	if err != nil {
		return fmt.Errorf("invalid email sender: %w", err)
	}
	recipients := make([]*mail.Address, len(messages))
	for i, message := range messages {
		if recipients[i], err = mail.ParseAddress(message.To); err != nil {
			return fmt.Errorf("invalid email recipient: %w", err)
		}
	}
	if len(messages) == 0 {
		return nil
	}
	host := m.addr
	if parsedHost, _, splitErr := strings.Cut(m.addr, ":"); splitErr {
		host = parsedHost
	}
	// The session gets the per-message timeout once for connecting and once
	// for each message.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(messages)+1)*emailSMTPTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: emailSMTPTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
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
	if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: m.rootCAs}); err != nil {
		return err
	}
	if m.username != "" || m.password != "" {
		if err := client.Auth(smtp.PlainAuth("", m.username, m.password, host)); err != nil {
			return err
		}
	}
	for i, message := range messages {
		if i > 0 {
			if err := client.Reset(); err != nil {
				return err
			}
		}
		if err := sendSMTPMessage(client, from, recipients[i], message); err != nil {
			return RecipientError{Address: recipients[i].Address, Err: err}
		}
	}
	if err := client.Quit(); err != nil {
		slog.Default().Warn("smtp quit failed after delivering every message", "error", err)
	}
	return nil
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
