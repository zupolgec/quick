package main

import (
	"bufio"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"testing"
)

// fakeSMTP accepts one message on a local port and hands back what it got.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { io.WriteString(c, s+"\r\n") }
		say("220 fake")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "DATA"):
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 ok")
				got <- data.String()
			case strings.HasPrefix(cmd, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSMTPMailerSends(t *testing.T) {
	addr, got := fakeSMTP(t)
	m, err := newSMTPMailer("smtp://"+addr, "quick <quick@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	link := "https://app.quick.test/__quick/email?t=" + strings.Repeat("x", 90)
	if err := m.Send("dave@client.it", "Il tuo link di accesso a app.quick.test", "Ciao,\n\n"+link+"\n"); err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(<-got))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("To") != "dave@client.it" || !strings.Contains(msg.Header.Get("From"), "quick@example.com") {
		t.Fatalf("headers: %v", msg.Header)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subj != "Il tuo link di accesso a app.quick.test" {
		t.Fatalf("subject %q", subj)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if !strings.Contains(string(body), link) {
		t.Fatalf("link not intact in body:\n%s", body)
	}
}

func TestMailerConfig(t *testing.T) {
	if m, err := newSMTPMailer("", ""); m != nil || err != nil {
		t.Fatal("unset should mean no email")
	}
	for _, c := range [][2]string{
		{"smtp://host:587", ""},
		{"", "quick@example.com"},
		{"http://host", "quick@example.com"},
		{"smtp://host", "not an address"},
	} {
		if _, err := newSMTPMailer(c[0], c[1]); err == nil {
			t.Fatalf("%q %q accepted", c[0], c[1])
		}
	}
}
