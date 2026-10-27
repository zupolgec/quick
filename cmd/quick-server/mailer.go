// Outgoing email (QUICK_SMTP_URL + QUICK_MAIL_FROM), used only for the
// sign-in links of outside people on "people" sites (guestlink.go). Unset:
// the server sends no email and sites can't be shared outside the company.
package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

type mailer interface {
	Send(m mailMessage) error
}

// mailMessage is sent as multipart/alternative: Text, then HTML.
type mailMessage struct {
	To, Subject, Text, HTML string
}

type smtpMailer struct {
	addr        string // host:port
	host        string
	implicitTLS bool // smtps://: TLS from the start; smtp://: STARTTLS when offered
	auth        smtp.Auth
	from        string // header value, e.g. "quick <quick@example.com>"
	fromAddr    string // envelope sender
}

// mailerFromEnv reads QUICK_SMTP_URL (smtp://user:pass@host:587 or
// smtps://user:pass@host:465) and QUICK_MAIL_FROM. Neither set: nil, no email.
func mailerFromEnv() (mailer, error) {
	return newSMTPMailer(strings.TrimSpace(os.Getenv("QUICK_SMTP_URL")), strings.TrimSpace(os.Getenv("QUICK_MAIL_FROM")))
}

func newSMTPMailer(raw, from string) (mailer, error) {
	if raw == "" && from == "" {
		return nil, nil
	}
	if raw == "" || from == "" {
		return nil, errors.New("QUICK_SMTP_URL and QUICK_MAIL_FROM go together")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "smtp" && u.Scheme != "smtps") {
		return nil, errors.New("QUICK_SMTP_URL: want smtp://user:pass@host:587 or smtps://user:pass@host:465")
	}
	a, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("QUICK_MAIL_FROM: %v", err)
	}
	m := &smtpMailer{host: u.Hostname(), implicitTLS: u.Scheme == "smtps", from: a.String(), fromAddr: a.Address}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "465", false: "587"}[m.implicitTLS]
	}
	m.addr = net.JoinHostPort(m.host, port)
	if u.User != nil {
		pass, _ := u.User.Password()
		// PlainAuth refuses to send the password without TLS (except to localhost).
		m.auth = smtp.PlainAuth("", u.User.Username(), pass, m.host)
	}
	return m, nil
}

func (m *smtpMailer) Send(msg mailMessage) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if m.implicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", m.addr, &tls.Config{ServerName: m.host})
	} else {
		conn, err = dialer.Dial("tcp", m.addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && !m.implicitTLS {
		if err := c.StartTLS(&tls.Config{ServerName: m.host}); err != nil {
			return err
		}
	}
	if m.auth != nil {
		if err := c.Auth(m.auth); err != nil {
			return err
		}
	}
	// 8bit keeps links whole in the raw message; quoted-printable (which
	// splits long lines) only for servers that don't take 8-bit bodies.
	eightBit, _ := c.Extension("8BITMIME")
	data, err := buildMail(m.from, m.fromAddr, msg, eightBit)
	if err != nil {
		return err
	}
	if err := c.Mail(m.fromAddr); err != nil {
		return err
	}
	if err := c.Rcpt(msg.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMail writes a multipart/alternative UTF-8 message (text, then HTML)
// with CRLF line endings.
func buildMail(from, fromAddr string, msg mailMessage, eightBit bool) ([]byte, error) {
	if strings.ContainsAny(msg.To+msg.Subject, "\r\n") {
		return nil, errors.New("invalid mail header")
	}
	_, domain, _ := strings.Cut(fromAddr, "@")
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from)
	hdr("To", msg.To)
	hdr("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+randomID(18)+"@"+domain+">")
	hdr("Auto-Submitted", "auto-generated")
	hdr("MIME-Version", "1.0")
	mw := multipart.NewWriter(&b)
	hdr("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	enc := "quoted-printable"
	if eightBit {
		enc = "8bit"
	}
	for _, part := range []struct{ ct, body string }{{"text/plain", msg.Text}, {"text/html", msg.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.ct + "; charset=utf-8"},
			"Content-Transfer-Encoding": {enc},
		})
		if err != nil {
			return nil, err
		}
		body := strings.ReplaceAll(strings.ReplaceAll(part.body, "\r\n", "\n"), "\n", "\r\n")
		if eightBit {
			io.WriteString(w, body)
		} else {
			qp := quotedprintable.NewWriter(w)
			io.WriteString(qp, body)
			qp.Close()
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
