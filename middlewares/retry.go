package middlewares

import (
	"bufio"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"

	"github.com/containous/traefik/log"
)

// Compile time validation that the response writer implements http interfaces correctly.
var _ Stateful = &retryResponseWriterWithCloseNotify{}

// Retry is a middleware that retries requests
type Retry struct {
	attempts int
	next     http.Handler
	listener RetryListener
}

// NewRetry returns a new Retry instance
func NewRetry(attempts int, next http.Handler, listener RetryListener) *Retry {
	return &Retry{
		attempts: attempts,
		next:     next,
		listener: listener,
	}
}

func (retry *Retry) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// if we might make multiple attempts, swap the body for an ioutil.NopCloser
	// cf https://github.com/containous/traefik/issues/1008
	if retry.attempts > 1 {
		body := r.Body
		defer body.Close()
		r.Body = ioutil.NopCloser(body)
	}

	attempts := 1
	for {
		attemptsExhausted := attempts >= retry.attempts
		// Websocket requests can't be retried at this point in time.
		// This is due to the fact that gorilla/websocket doesn't use the request
		// context and so we don't get httptrace information.
		// Websocket clients should however retry on their own anyway.
		shouldRetry := !attemptsExhausted && !isWebsocketRequest(r)
		retryResponseWriter := newRetryResponseWriter(rw, shouldRetry)

		// Disable retries when the backend already received request data
		trace := &httptrace.ClientTrace{
			WroteHeaders: func() {
				retryResponseWriter.DisableRetries()
			},
			WroteRequest: func(httptrace.WroteRequestInfo) {
				retryResponseWriter.DisableRetries()
			},
		}
		newCtx := httptrace.WithClientTrace(r.Context(), trace)

		retry.next.ServeHTTP(retryResponseWriter, r.WithContext(newCtx))
		if !retryResponseWriter.ShouldRetry() {
			break
		}

		attempts++
		log.Debugf("New attempt %d for request: %v", attempts, r.URL)
		retry.listener.Retried(r, attempts)
	}
}

// RetryListener is used to inform about retry attempts.
type RetryListener interface {
	// Retried will be called when a retry happens, with the request attempt passed to it.
	// For the first retry this will be attempt 2.
	Retried(req *http.Request, attempt int)
}

// RetryListeners is a convenience type to construct a list of RetryListener and notify
// each of them about a retry attempt.
type RetryListeners []RetryListener

// Retried exists to implement the RetryListener interface. It calls Retried on each of its slice entries.
func (l RetryListeners) Retried(req *http.Request, attempt int) {
	for _, retryListener := range l {
		retryListener.Retried(req, attempt)
	}
}

type retryResponseWriter interface {
	http.ResponseWriter
	http.Flusher
	ShouldRetry() bool
	DisableRetries()
}

func newRetryResponseWriter(rw http.ResponseWriter, shouldRetry bool) retryResponseWriter {
	responseWriter := &retryResponseWriterWithoutCloseNotify{
		responseWriter: rw,
		shouldRetry:    shouldRetry,
	}
	if _, ok := rw.(http.CloseNotifier); ok {
		return &retryResponseWriterWithCloseNotify{responseWriter}
	}
	return responseWriter
}

type retryResponseWriterWithoutCloseNotify struct {
	responseWriter http.ResponseWriter
	shouldRetry    bool
}

func (rr *retryResponseWriterWithoutCloseNotify) ShouldRetry() bool {
	return rr.shouldRetry
}

func (rr *retryResponseWriterWithoutCloseNotify) DisableRetries() {
	rr.shouldRetry = false
}

func (rr *retryResponseWriterWithoutCloseNotify) Header() http.Header {
	if rr.ShouldRetry() {
		return make(http.Header)
	}
	return rr.responseWriter.Header()
}

func (rr *retryResponseWriterWithoutCloseNotify) Write(buf []byte) (int, error) {
	if rr.ShouldRetry() {
		return 0, nil
	}
	return rr.responseWriter.Write(buf)
}

func (rr *retryResponseWriterWithoutCloseNotify) WriteHeader(code int) {
	if rr.ShouldRetry() && code == http.StatusServiceUnavailable {
		// We get a 503 HTTP Status Code when there is no backend server in the pool
		// to which the request could be sent.  Also, note that rr.ShouldRetry()
		// will never return true in case there was a connection established to
		// the backend server and so we can be sure that the 503 was produced
		// inside Traefik already and we don't have to retry in this cases.
		rr.DisableRetries()
	}

	if rr.ShouldRetry() {
		return
	}
	rr.responseWriter.WriteHeader(code)
}

func (rr *retryResponseWriterWithoutCloseNotify) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return rr.responseWriter.(http.Hijacker).Hijack()
}

func (rr *retryResponseWriterWithoutCloseNotify) Flush() {
	if flusher, ok := rr.responseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type retryResponseWriterWithCloseNotify struct {
	*retryResponseWriterWithoutCloseNotify
}

func (rr *retryResponseWriterWithCloseNotify) CloseNotify() <-chan bool {
	return rr.responseWriter.(http.CloseNotifier).CloseNotify()
}
