package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const smtpTimeout = 20 * time.Second

// SMTP sends one plain-text mail per event. The whole exchange is bounded
// by dial timeouts and connection deadlines: a stalled server must never
// leak the send goroutine or its socket in a long-running daemon.
type SMTP struct {
	Host     string
	Port     int
	From     string
	To       []string
	Username string
	Password string

	// sendMail is a seam for tests; defaults to the deadline-bound client.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func NewSMTP(host string, port int, from string, to []string, username, password string) *SMTP {
	s := &SMTP{
		Host: host, Port: port, From: from, To: to,
		Username: username, Password: password,
	}
	s.sendMail = s.deadlineSendMail
	return s
}

func (s *SMTP) Notify(ctx context.Context, ev Event) error {
	// net/smtp has no context support; the connection deadlines below bound
	// the goroutine's lifetime, and ctx bounds how long the caller waits.
	done := make(chan error, 1)
	go func() {
		done <- s.sendMail(net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port)), s.auth(), s.From, s.To, s.message(ev))
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SMTP) auth() smtp.Auth {
	if s.Username == "" {
		return nil
	}
	return smtp.PlainAuth("", s.Username, s.Password, s.Host)
}

// deadlineSendMail is smtp.SendMail with a dial timeout and a hard deadline
// on every read/write, plus STARTTLS when the server offers it.
func (s *SMTP) deadlineSendMail(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		_ = conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return err
		}
	}
	if a != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(a); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (s *SMTP) message(ev Event) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(s.To, ", "))
	fmt.Fprintf(&b, "Subject: [plico] %s: %s\r\n", ev.Stack, ev.Type)
	fmt.Fprintf(&b, "Date: %s\r\n", ev.Time.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(crlf(formatBody(ev)))
	return []byte(b.String())
}

// crlf normalizes any mix of \r\n, \r and \n to strict CRLF: bare CR bytes
// in hook output would be rejected by strict MTAs (SMTP smuggling
// hardening), losing exactly the failure mails that carry diagnostics.
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
