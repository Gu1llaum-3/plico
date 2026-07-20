package notify

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTP is a minimal in-process SMTP server for exercising the real
// deadlineSendMail path (no TLS). advertiseAuth controls whether the EHLO
// response offers the AUTH extension.
type fakeSMTP struct {
	ln            net.Listener
	advertiseAuth bool
	gotAuth       bool
	dataReceived  bool
}

func newFakeSMTP(t *testing.T, advertiseAuth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, advertiseAuth: advertiseAuth}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s)) }

	write("220 fake ESMTP\r\n")
	inData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case inData:
			if line == "." {
				inData = false
				f.dataReceived = true
				write("250 OK\r\n")
			}
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			if f.advertiseAuth {
				write("250-fake\r\n250 AUTH PLAIN LOGIN\r\n")
			} else {
				write("250-fake\r\n250 SIZE 10485760\r\n")
			}
		case strings.HasPrefix(line, "AUTH"):
			f.gotAuth = true
			write("235 authenticated\r\n")
		case strings.HasPrefix(line, "MAIL"):
			write("250 OK\r\n")
		case strings.HasPrefix(line, "RCPT"):
			write("250 OK\r\n")
		case line == "DATA":
			inData = true
			write("354 send data\r\n")
		case line == "QUIT":
			write("221 bye\r\n")
			return
		default:
			write("250 OK\r\n")
		}
	}
}

func TestSMTPErrorsWhenAuthConfiguredButNotAdvertised(t *testing.T) {
	t.Parallel()
	f := newFakeSMTP(t, false) // server does NOT advertise AUTH
	host, port := f.hostPort()
	s := NewSMTP(host, port, "plico@x", []string{"ops@x"}, "user", "pass")

	err := s.Notify(context.Background(), Event{Type: DeployFailed, Stack: "web", Time: time.Now()})
	if err == nil {
		t.Fatal("configured credentials against a no-AUTH server must error, not send unauthenticated")
	}
	if !strings.Contains(err.Error(), "AUTH") {
		t.Errorf("error should mention AUTH, got %v", err)
	}
}

func TestSMTPSendsWhenAuthAdvertised(t *testing.T) {
	t.Parallel()
	f := newFakeSMTP(t, true)
	host, port := f.hostPort()
	s := NewSMTP(host, port, "plico@x", []string{"ops@x"}, "user", "pass")

	if err := s.Notify(context.Background(), Event{Type: DeployFailed, Stack: "web", Time: time.Now()}); err != nil {
		t.Fatalf("send failed against a healthy server: %v", err)
	}
	if !f.gotAuth {
		t.Error("server never saw AUTH")
	}
	if !f.dataReceived {
		t.Error("server never received the message body")
	}
}

func TestSMTPNoAuthWhenUnconfigured(t *testing.T) {
	t.Parallel()
	f := newFakeSMTP(t, false) // no AUTH advertised, and none configured
	host, port := f.hostPort()
	s := NewSMTP(host, port, "plico@x", []string{"ops@x"}, "", "")

	if err := s.Notify(context.Background(), Event{Type: DeployFailed, Stack: "web", Time: time.Now()}); err != nil {
		t.Fatalf("send without configured auth should succeed: %v", err)
	}
	if f.gotAuth {
		t.Error("no auth was configured, the client must not attempt AUTH")
	}
}
