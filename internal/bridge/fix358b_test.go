package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryDecisionTruthTable(t *testing.T) {
	for name, c := range map[string]struct {
		sent, rejected, other int32
		dead, want            bool
	}{
		"nothing written, session already dead": {0, 0, 0, true, true},
		"nothing written, not dead":             {0, 0, 0, false, false},
		"own POST answered 404":                 {1, 1, 0, true, true},
		"own POST answered 404, not dead":       {1, 1, 0, false, true},
		"written, no outcome (in flight)":       {1, 0, 0, true, false},
		"written, other outcome":                {1, 0, 1, true, false},
		"404 plus another outcome":              {1, 1, 1, true, false},
		"two POSTs":                             {2, 1, 0, true, false},
	} {
		var tr callTrace
		tr.sent.Store(c.sent)
		tr.rejected.Store(c.rejected)
		tr.other.Store(c.other)
		if got := tr.mayResend(c.dead); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestRemoteTransportIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	tr := newRemoteTransport(true, false, nil)
	if tr.Proxy != nil {
		t.Fatal("proxy environment variables would bypass the address policy")
	}
	if tr.DialContext == nil {
		t.Fatal("no dial-time address check")
	}
}

// A server that never answers the session DELETE is released only by an explicit Abort, never by a timer.
func TestAbortReleasesStuckTeardown(t *testing.T) {
	s := newStub(t)
	stuck := make(chan struct{})
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			close(stuck)
			<-r.Context().Done()
			return
		}
		s.ServeHTTP(w, r)
	})
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	closed := make(chan struct{})
	go func() { g.Close(); close(closed) }()
	<-stuck
	select {
	case <-closed:
		t.Fatal("Close returned while the server still held the DELETE")
	case <-time.After(300 * time.Millisecond):
	}
	g.Abort()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Abort did not release the teardown")
	}
}

var _ = httptest.NewServer
