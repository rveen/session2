package session2

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"sync"
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
// Capacity / eviction
// -------------------------------------------------------------------------

func TestMaxSessionsEviction(t *testing.T) {
	const max = 100
	Init(Options{AllowHTTP: true, CleanInterval: time.Hour, MaxSessions: max})
	defer Close()

	var added []*Session
	for i := 0; i < 2*max; i++ {
		s := NewSession()
		Add(s, httptest.NewRecorder())
		added = append(added, s)
	}

	if n := Len(); n > max {
		t.Errorf("Len() = %d, want <= %d", n, max)
	}

	// The most recently added session must have survived: eviction takes the
	// least recently accessed, and nothing has touched the newcomer since.
	last := added[len(added)-1]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sessid", Value: last.ID()})
	if Get(req) == nil {
		t.Error("most recently added session was evicted")
	}
}

func TestMaxSessionsUnlimited(t *testing.T) {
	Init(Options{AllowHTTP: true, CleanInterval: time.Hour, MaxSessions: 0})
	defer Close()

	for i := 0; i < 500; i++ {
		Add(NewSession(), httptest.NewRecorder())
	}
	eq(t, 500, Len())
}

func TestSetMaxSessions(t *testing.T) {
	Init(Options{AllowHTTP: true, CleanInterval: time.Hour})
	defer Close()

	for i := 0; i < 50; i++ {
		Add(NewSession(), httptest.NewRecorder())
	}
	eq(t, 50, Len())

	// Tightening the cap does not evict what is already stored...
	SetMaxSessions(10)
	eq(t, 50, Len())

	// ...but the next Add brings the store down to the cap.
	Add(NewSession(), httptest.NewRecorder())
	if n := Len(); n > 10 {
		t.Errorf("Len() = %d, want <= 10 after Add under the new cap", n)
	}
}

// -------------------------------------------------------------------------
// Map compaction
// -------------------------------------------------------------------------

// TestCompactionReleasesBuckets drives compactLocked directly rather than
// waiting on the cleaner: expiry is wall-clock driven and inserting 100k
// sessions under -race is slow enough to make that racy.
func TestCompactionReleasesBuckets(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 100k sessions")
	}
	const n = 100000

	m := newManager(Options{AllowHTTP: true, CleanInterval: time.Hour})
	defer m.Close()

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}

	base := heap()
	for i := 0; i < n; i++ {
		m.Add(NewSession(), httptest.NewRecorder())
	}
	eq(t, n, m.Len())
	peak := heap()

	// Simulate the cleaner having expired every session.
	m.mu.Lock()
	for id := range m.sessions {
		delete(m.sessions, id)
	}
	m.compactLocked()
	m.mu.Unlock()
	eq(t, 0, m.Len())
	after := heap()

	// Without compaction the emptied map retains its bucket array (~3.5 MB at
	// 100k). Allow a generous margin: we only care that it is not retained.
	live := peak - base
	retained := after - base
	if retained > live/10 {
		t.Errorf("map not compacted: %d bytes retained of %d live (>10%%)", retained, live)
	}
}

func TestCompactionSkipsSmallMaps(t *testing.T) {
	m := newManager(Options{AllowHTTP: true, CleanInterval: time.Hour})
	defer m.Close()

	for i := 0; i < 100; i++ {
		m.Add(NewSession(), httptest.NewRecorder())
	}
	m.mu.Lock()
	before := reflect.ValueOf(m.sessions).Pointer()
	m.compactLocked()
	after := reflect.ValueOf(m.sessions).Pointer()
	m.mu.Unlock()

	// highWater is 100, below the 1024 floor, so the map must not be rebuilt.
	eq(t, before, after)
	eq(t, 100, m.Len())
}

// -------------------------------------------------------------------------
// Concurrency
// -------------------------------------------------------------------------

// TestInitRace exercises the atomic swap of the global manager against
// concurrent readers. Meaningful under -race.
func TestInitRace(t *testing.T) {
	Init(Options{AllowHTTP: true, CleanInterval: time.Hour})
	defer Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			for {
				select {
				case <-stop:
					return
				default:
				}
				Add(NewSession(), httptest.NewRecorder())
				Get(req)
				Len()
			}
		}()
	}

	for i := 0; i < 20; i++ {
		Init(Options{AllowHTTP: true, CleanInterval: time.Hour})
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
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
