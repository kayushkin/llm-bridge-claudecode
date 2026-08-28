package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// pathProbeIDs is the id table this test drives the emitter with. Each entry
// carries a character that changes which endpoint the request addresses if it
// reaches the path unescaped.
//
// "well formed" and "space" are expected to pass even on unrepaired code:
// Go's url.Parse already escapes a raw space. The space probe is not a
// defect-detector — it is the only thing separating url.PathEscape from
// url.QueryEscape, which encodes a space as '+'. A probe set pruned by how
// many rows it reddens would drop exactly the probe the scorer needs.
var pathProbeIDs = []struct {
	name string
	id   string
}{
	{"well formed", "br_01HXYZ"},
	{"fragment", "br#frag"},
	{"query", "br?replay=1"},
	{"extra segment", "a/b"},
	{"climbs out of the collection", "../sessions"},
	{"space", "br one"},
	{"already percent encoded", "br%2Fb"},
}

// TestBridgeSessionIDStaysOnePathSegment pins that the bridge session id the
// sidecar is spawned with occupies exactly one path segment on the wire.
//
// The id is caller-minted and unvalidated: bridge-server's POST /sessions
// copies the request body's session_id verbatim, and bridge-server answers
// unauthenticated on *:8160. So this is not a defensive nicety about a
// well-formed "br_<nanos>" — the value is chosen by whoever opened the
// session.
//
// It asserts r.RequestURI, NOT r.URL.Path. Go's server has already decoded
// %2F back to a slash by the time it fills URL.Path, so a URL.Path assertion
// reads identically whether or not the client escaped anything — it cannot
// hold this property at all.
func TestBridgeSessionIDStaysOnePathSegment(t *testing.T) {
	for _, probe := range pathProbeIDs {
		t.Run(probe.name, func(t *testing.T) {
			got := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.RequestURI
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			emit := newSidecarEmitter(srv.URL, probe.id)
			emit(msg.Event{Type: msg.EventBlock})

			want := "/sidecar/event/" + url.PathEscape(probe.id)
			select {
			case addressed := <-got:
				if addressed != want {
					t.Errorf("id %q addressed %q, want %q", probe.id, addressed, want)
				}
			default:
				t.Fatalf("id %q: emitter sent no request at all", probe.id)
			}
		})
	}
}
