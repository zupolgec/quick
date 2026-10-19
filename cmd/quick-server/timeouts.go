// Time bounds for deploy requests. A client that opens a deploy request and
// stops sending must not hold anything forever: a request in flight keeps its
// lease alive, a commit or tar upload holds the site lock while it reads its
// body, a refused request is read for a while before the 423.
//
//   - Every deploy body read has a progress deadline (idle: no bytes for
//     that long aborts it) and an overall cap from the start of the request
//     (manifest for plan/commit, upload for chunk batches and tar uploads).
//     A timed-out read fails the handler like any other bad body, so its
//     defers run: the lease's in-flight count drops, the site lock is
//     released.
//   - A refused request's body is read at most refuseDrainBytes, for at
//     most drain, then answered (a bigger upload may see the connection
//     closed instead of the 423; the CLI probes for it).
//   - Every response write of a deploy endpoint has a deadline (write), so
//     a client that stops reading can't keep a handler, and its lease in
//     flight, forever. A plan whose answer can't be delivered releases the
//     lease it opened (the client never learned the id).
//   - The http.Server bounds headers and idle keep-alive connections.
package main

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// deployTimeouts are per server so tests can shorten them; zero = default.
type deployTimeouts struct {
	idle     time.Duration // no body bytes for this long (default 30s)
	manifest time.Duration // whole plan/commit body (default 2m)
	upload   time.Duration // whole chunk batch or tar upload (default 30m)
	drain    time.Duration // refused request's body (default 5s)
	write    time.Duration // each response write (default 30s)
}

// refuseDrainBytes caps how much of a refused request's body is read.
const refuseDrainBytes = 1 << 20

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (t deployTimeouts) idleOr() time.Duration     { return orDefault(t.idle, 30*time.Second) }
func (t deployTimeouts) manifestOr() time.Duration { return orDefault(t.manifest, 2*time.Minute) }
func (t deployTimeouts) uploadOr() time.Duration   { return orDefault(t.upload, 30*time.Minute) }
func (t deployTimeouts) drainOr() time.Duration    { return orDefault(t.drain, 5*time.Second) }
func (t deployTimeouts) writeOr() time.Duration    { return orDefault(t.write, 30*time.Second) }

// deadlineBody pushes the connection's read deadline forward before every
// read: at most idle from now, never past end.
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	end  time.Time
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	d := time.Now().Add(b.idle)
	if d.After(b.end) {
		d = b.end
	}
	_ = b.rc.SetReadDeadline(d) // unsupported (tests' recorders): no deadline
	return b.ReadCloser.Read(p)
}

// deadlineWriter gives every response write a deadline: a client that stops
// reading (a big plan answer) can't block the handler, and with it the
// lease's in-flight count, forever. Deploy endpoints only: static files and
// proxied responses keep no write deadline.
type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
	d  time.Duration
}

func (w *deadlineWriter) WriteHeader(code int) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.d)) // covers the final flush too
	w.ResponseWriter.WriteHeader(code)
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.d))
	return w.ResponseWriter.Write(p)
}

func (w *deadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// boundIO puts r's body under the idle deadline and an overall cap of total
// from now, and the response under the write deadline. Call before anything
// reads the body or writes; use the returned writer.
func (s *server) boundIO(w http.ResponseWriter, r *http.Request, total time.Duration) http.ResponseWriter {
	rc := http.NewResponseController(w)
	r.Body = &deadlineBody{
		ReadCloser: r.Body,
		rc:         rc,
		idle:       s.timeouts.idleOr(),
		end:        time.Now().Add(total),
	}
	return &deadlineWriter{ResponseWriter: w, rc: rc, d: s.timeouts.writeOr()}
}

// flushed sends what is buffered of the response, so a failed delivery shows
// up as an error (writers that can't flush: assume delivered).
func flushed(w http.ResponseWriter) error {
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// drainRefused reads a refused request's body, bounded in bytes and time.
func (s *server) drainRefused(r *http.Request) {
	end := time.Now().Add(s.timeouts.drainOr())
	if b, ok := r.Body.(*deadlineBody); ok && end.Before(b.end) {
		b.end = end
	}
	_, _ = io.CopyN(io.Discard, r.Body, refuseDrainBytes)
}

// newHTTPServer is the server main listens with.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}
