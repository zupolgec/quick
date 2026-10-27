package main

import (
	"bufio"
	"io"
	"mime"
	"mime/multipart"
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
				say("250-fake")
				say("250 8BITMIME")
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
	err = m.Send(mailMessage{To: "dave@client.it", Subject: "Il tuo link di accesso a app.quick.test",
		Text: "Ciao, è qui:\n\n" + link + "\n", HTML: `<p>Ciao, è qui: <a href="` + link + `">apri</a></p>`})
	if err != nil {
		t.Fatal(err)
	}
	raw := <-got
	// the link stays whole in the raw message: no soft line breaks or =3D.
	if strings.Count(raw, link) != 2 {
		t.Fatalf("link not intact in both parts:\n%s", raw)
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
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
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q", msg.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(part)
		types = append(types, part.Header.Get("Content-Type"))
		if !strings.Contains(string(body), "è qui") {
			t.Errorf("%s part: %q", part.Header.Get("Content-Type"), body)
		}
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("parts %v, want text then html", types)
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
