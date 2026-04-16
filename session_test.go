package session2

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// eq / neq are simple test helpers that replace the mighty dependency.
func eq(t *testing.T, want, got interface{}) {
	t.Helper()
	if want != got {
		t.Errorf("want %v, got %v", want, got)
	}
}

func neq(t *testing.T, a, b interface{}) {
	t.Helper()
	if a == b {
		t.Errorf("expected values to differ, both are %v", a)
	}
}

// -------------------------------------------------------------------------
// Session
// -------------------------------------------------------------------------

func TestNewSession(t *testing.T) {
	s := NewSession()
	eq(t, s.Created(), s.Accessed())
	eq(t, 0, len(s.Attrs()))
	eq(t, 30*time.Minute, s.Timeout())

	time.Sleep(10 * time.Millisecond)
	s.touch()
	neq(t, s.Created(), s.Accessed())
}

func TestSessOptions(t *testing.T) {
	s := NewSession(SessOptions{
		Attrs:   map[string]interface{}{"a": 1},
		Timeout: 43 * time.Minute,
	})
	eq(t, 1, s.Attr("a"))
	eq(t, 43*time.Minute, s.Timeout())
}

func TestSessionAttrs(t *testing.T) {
	s := NewSession()

	eq(t, nil, s.Attr("a"))
	s.SetAttr("a", 1)
	eq(t, 1, s.Attr("a"))
	eq(t, 1, len(s.Attrs()))

	s.SetAttr("a", nil)
	eq(t, nil, s.Attr("a"))
	eq(t, 0, len(s.Attrs()))
}

// -------------------------------------------------------------------------
// Manager storage operations
// -------------------------------------------------------------------------

func TestManagerAddGetRemove(t *testing.T) {
	Init(Options{AllowHTTP: true, CleanInterval: time.Hour})
	defer Close()

	// Get on unknown ID returns nil
	req := httptest.NewRequest("GET", "/", nil)
	eq(t, (*Session)(nil), Get(req))

	// Add a session, then retrieve it via a fake request carrying the cookie
	sess := NewSession()
	w := httptest.NewRecorder()
	Add(sess, w)
	eq(t, 1, Len())

	// Build a request that carries the cookie set by Add
	req2 := httptest.NewRequest("GET", "/", nil)
	for _, c := range w.Result().Cookies() {
		req2.AddCookie(c)
	}

	time.Sleep(5 * time.Millisecond)
	got := Get(req2)
	neq(t, (*Session)(nil), got)
	neq(t, got.Created(), got.Accessed()) // touch() updated accessed time

	// Remove
	w2 := httptest.NewRecorder()
	Remove(got, w2)
	eq(t, 0, Len())

	req3 := httptest.NewRequest("GET", "/", nil)
	for _, c := range w.Result().Cookies() {
		req3.AddCookie(c)
	}
	eq(t, (*Session)(nil), Get(req3))
}

// -------------------------------------------------------------------------
// Cleaner / expiry
// -------------------------------------------------------------------------

func TestManagerCleaner(t *testing.T) {
	Init(Options{
		AllowHTTP:     true,
		CleanInterval: 10 * time.Millisecond,
	})
	defer Close()

	sess := NewSession(SessOptions{Timeout: 50 * time.Millisecond})
	w := httptest.NewRecorder()
	Add(sess, w)
	eq(t, 1, Len())

	time.Sleep(30 * time.Millisecond)
	eq(t, 1, Len()) // not expired yet

	time.Sleep(80 * time.Millisecond)
	eq(t, 0, Len()) // cleaner removed it
}

// -------------------------------------------------------------------------
// Global integration
// -------------------------------------------------------------------------

func globalHandler(w http.ResponseWriter, r *http.Request) {
	if sess := Get(r); sess == nil {
		sess = NewSession()
		sess.SetAttr("counter", 1)
		Add(sess, w)
		w.Header().Set("test", "0")
	} else {
		if sess.Attr("counter") == 1 {
			sess.SetAttr("counter", 2)
			w.Header().Set("test", "1")
		} else {
			Remove(sess, w)
			w.Header().Set("test", "2")
		}
	}
}

func TestGlobal(t *testing.T) {
	Init(Options{AllowHTTP: true, CookieMaxAge: time.Hour})
	defer Close()

	server := httptest.NewServer(http.HandlerFunc(globalHandler))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	eq(t, nil, err)

	client := &http.Client{Jar: jar}

	// 3 iterations: Create, Change, Remove session.
	// 4th request: session was removed, so it creates a new one again.
	for i := 0; i < 4; i++ {
		resp, err := client.Get(server.URL)
		eq(t, nil, err)
		eq(t, strconv.Itoa(i%3), resp.Header.Get("test"))
		resp.Body.Close()
	}
}
