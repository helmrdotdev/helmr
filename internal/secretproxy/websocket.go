package secretproxy

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
)

// WebSocket support is an HTTP/1.1 handshake followed by opaque bytes. The
// endpoints retain ownership of frame validation, extensions and close frames.
func websocketRequest(r *http.Request) bool {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") && connectionNominates(r.Header, name) {
			return false
		}
	}
	key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	return r.ProtoMajor == 1 && r.ProtoMinor == 1 && r.Method == http.MethodGet &&
		connectionNominates(r.Header, "Upgrade") &&
		len(r.Header.Values("Upgrade")) == 1 && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		len(r.Header.Values("Sec-WebSocket-Version")) == 1 && r.Header.Get("Sec-WebSocket-Version") == "13" &&
		len(r.Header.Values("Sec-WebSocket-Key")) == 1 && err == nil && len(key) == 16 &&
		r.ContentLength == 0 && len(r.TransferEncoding) == 0 && len(r.Trailer) == 0
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// forwardWebSocket retains the handler and its admission until both relay
// directions have exited. In particular, hijacking is not handler completion.
func forwardWebSocket(w http.ResponseWriter, r *http.Request, transport *http.Transport) (hijacked bool) {
	response, err := transport.RoundTrip(r)
	if err != nil {
		deny(w)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		stripHopHeaders(response.Header)
		for name, values := range response.Header {
			w.Header()[name] = values
		}
		for name := range response.Trailer {
			w.Header().Add("Trailer", name)
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(flushingWriter{w}, response.Body)
		for name, values := range response.Trailer {
			w.Header()[http.TrailerPrefix+name] = values
		}
		return
	}
	upstream, ok := response.Body.(io.ReadWriteCloser)
	if !ok || !connectionNominates(response.Header, "Upgrade") ||
		len(response.Header.Values("Upgrade")) != 1 || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		len(response.Header.Values("Sec-WebSocket-Accept")) != 1 || response.Header.Get("Sec-WebSocket-Accept") != websocketAccept(r.Header.Get("Sec-WebSocket-Key")) {
		deny(w)
		return
	}
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		deny(w)
		return
	}
	hijacked = true
	defer client.Close()
	// Cancellation must interrupt the initial 101 write as well as either copy.
	// Joining the callback prevents a close operation from escaping this owner.
	canceled := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		client.Close()
		upstream.Close()
		close(canceled)
	})
	defer func() {
		if !stop() {
			<-canceled
		}
	}()
	stripHopHeaders(response.Header)
	response.Header.Set("Connection", "Upgrade")
	response.Header.Set("Upgrade", "websocket")
	response.Body = nil
	if err := response.Write(buffered); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buffered); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	// WebSocket has no useful transport half-close: its closing handshake is
	// carried in frames. Either transport EOF/error ends this opaque relay.
	client.Close()
	upstream.Close()
	<-done
	return
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
