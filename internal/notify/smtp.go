package notify

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP sends one plain-text mail per event. It uses the standard library
// client: STARTTLS is negotiated automatically when the server offers it,
// and PLAIN authentication is only attempted over TLS (or on localhost).
type SMTP struct {
	Host     string
	Port     int
	From     string
	To       []string
	Username string
	Password string

	// sendMail is a seam for tests; defaults to smtp.SendMail.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func NewSMTP(host string, port int, from string, to []string, username, password string) *SMTP {
	return &SMTP{
		Host: host, Port: port, From: from, To: to,
		Username: username, Password: password,
		sendMail: smtp.SendMail,
	}
}

func (s *SMTP) Notify(ctx context.Context, ev Event) error {
	// net/smtp has no context support; honor cancellation around the call.
	done := make(chan error, 1)
	go func() { done <- s.send(ev) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SMTP) send(ev Event) error {
	addr := net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port))
	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}
	msg := s.message(ev)
	return s.sendMail(addr, auth, s.From, s.To, msg)
}

func (s *SMTP) message(ev Event) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(s.To, ", "))
	fmt.Fprintf(&b, "Subject: [plico] %s: %s\r\n", ev.Stack, ev.Type)
	fmt.Fprintf(&b, "Date: %s\r\n", ev.Time.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(formatBody(ev), "\n", "\r\n"))
	return []byte(b.String())
}
