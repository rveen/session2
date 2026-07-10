// Package session provides easy-to-use, secure HTTP session management.
package session2

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Session holds the data for a single HTTP session.
// All methods are safe for concurrent use.
type Session struct {
	id       string
	created  time.Time
	accessed time.Time
	timeout  time.Duration
	attrs    map[string]interface{}
	mu       sync.RWMutex
}

// SessOptions defines options for NewSession. All fields are optional.
type SessOptions struct {
	// Initial attributes stored in the session.
	Attrs map[string]interface{}
	// Session timeout. Default: 30 minutes.
	Timeout time.Duration
}

// NewSession creates a new Session. Pass a SessOptions value to override defaults.
func NewSession(o ...SessOptions) *Session {
	var opts SessOptions
	if len(o) > 0 {
		opts = o[0]
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	b := make([]byte, 18)
	io.ReadFull(rand.Reader, b) //nolint:errcheck
	now := time.Now()
	s := &Session{
		id:       base64.URLEncoding.EncodeToString(b),
		created:  now,
		accessed: now,
		timeout:  timeout,
		attrs:    make(map[string]interface{}, len(opts.Attrs)),
	}
	for k, v := range opts.Attrs {
		s.attrs[k] = v
	}
	return s
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Created returns the time the session was created.
func (s *Session) Created() time.Time { return s.created }

// Accessed returns the time the session was last accessed.
func (s *Session) Accessed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accessed
}

// Timeout returns the inactivity duration after which the session expires.
func (s *Session) Timeout() time.Duration { return s.timeout }

// Attr returns the value of a stored attribute, or nil if not set.
func (s *Session) Attr(name string) interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attrs[name]
}

// SetAttr sets an attribute value. Pass nil to delete the attribute.
func (s *Session) SetAttr(name string, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == nil {
		delete(s.attrs, name)
	} else {
		s.attrs[name] = value
	}
}

// Attrs returns a copy of all attributes.
func (s *Session) Attrs() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]interface{}, len(s.attrs))
	for k, v := range s.attrs {
		m[k] = v
	}
	return m
}

func (s *Session) touch() {
	s.mu.Lock()
	s.accessed = time.Now()
	s.mu.Unlock()
}

// -------------------------------------------------------------------------
// Manager (internal)
// -------------------------------------------------------------------------

// Options configures the session manager. All fields are optional.
type Options struct {
	// Name of the session ID cookie. Default: "sessid".
	CookieName string
	// Allow session cookies over plain HTTP. Default: false (HTTPS only).
	AllowHTTP bool
	// Max age of the session cookie. Default: 30 days.
	// Set to a negative value for a session-scoped cookie (expires when browser closes).
	CookieMaxAge time.Duration
	// Cookie path. Default: "/".
	CookiePath string
	// How often the store is checked for expired sessions. Default: 10s.
	CleanInterval time.Duration
	// Maximum number of stored sessions. 0 means unlimited. When the store is
	// full, Add evicts the least recently accessed sessions to make room, so
	// adding a session never fails.
	MaxSessions int
}

type manager struct {
	opts     Options
	mu       sync.RWMutex
	sessions map[string]*Session
	// maxSessions mirrors opts.MaxSessions but may be changed after Init via
	// SetMaxSessions. Guarded by mu.
	maxSessions int
	// highWater is the largest size sessions has reached since the last
	// compaction. Go maps never shrink their bucket array on delete, so the
	// cleaner rebuilds the map once occupancy drops far enough below it.
	highWater int
	done      chan struct{}
	closeOnce sync.Once
}

func newManager(o Options) *manager {
	if o.CookieName == "" {
		o.CookieName = "sessid"
	}
	if o.CookieMaxAge == 0 {
		o.CookieMaxAge = 30 * 24 * time.Hour
	}
	if o.CookiePath == "" {
		o.CookiePath = "/"
	}
	if o.CleanInterval == 0 {
		o.CleanInterval = 10 * time.Second
	}
	m := &manager{
		opts:        o,
		sessions:    make(map[string]*Session),
		maxSessions: o.MaxSessions,
		done:        make(chan struct{}),
	}
	go m.cleaner()
	return m
}

func (m *manager) cleaner() {
	ticker := time.NewTicker(m.opts.CleanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-ticker.C:
			m.mu.Lock()
			for id, s := range m.sessions {
				if now.Sub(s.Accessed()) > s.Timeout() {
					delete(m.sessions, id)
				}
			}
			m.compactLocked()
			m.mu.Unlock()
		}
	}
}

// compactLocked rebuilds the session map once it has emptied out well below its
// high-water mark. Deleting from a Go map leaves the bucket array allocated, so
// without this a single traffic spike would raise the process floor for good.
func (m *manager) compactLocked() {
	n := len(m.sessions)
	if m.highWater <= 1024 || n*4 >= m.highWater {
		return
	}
	fresh := make(map[string]*Session, n)
	for id, s := range m.sessions {
		fresh[id] = s
	}
	m.sessions = fresh
	m.highWater = n
}

// evictLRULocked drops the least recently accessed sessions to make room for
// one more. It removes a batch rather than a single entry so that the O(n) scan
// is amortized over many inserts once the store sits at its cap.
func (m *manager) evictLRULocked() {
	// Enough to seat the incoming session, which is at least one and may be
	// many if SetMaxSessions has just lowered the cap.
	batch := len(m.sessions) - m.maxSessions + 1
	if b := m.maxSessions/64 + 1; b > batch {
		batch = b
	}

	type entry struct {
		id       string
		accessed time.Time
	}
	entries := make([]entry, 0, len(m.sessions))
	for id, s := range m.sessions {
		entries = append(entries, entry{id, s.Accessed()})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].accessed.Before(entries[j].accessed)
	})

	if batch > len(entries) {
		batch = len(entries)
	}
	for _, e := range entries[:batch] {
		delete(m.sessions, e.id)
	}
}

func (m *manager) Get(r *http.Request) *Session {
	c, err := r.Cookie(m.opts.CookieName)
	if err != nil {
		return nil
	}
	m.mu.RLock()
	sess := m.sessions[c.Value]
	m.mu.RUnlock()
	if sess != nil {
		sess.touch()
	}
	return sess
}

func (m *manager) Add(sess *Session, w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.opts.CookieName,
		Value:    sess.ID(),
		Path:     m.opts.CookiePath,
		HttpOnly: true,
		Secure:   !m.opts.AllowHTTP,
		MaxAge:   int(m.opts.CookieMaxAge.Seconds()),
	})
	m.mu.Lock()
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		m.evictLRULocked()
	}
	m.sessions[sess.ID()] = sess
	if len(m.sessions) > m.highWater {
		m.highWater = len(m.sessions)
	}
	m.mu.Unlock()
}

// SetMaxSessions changes the cap after Init. 0 means unlimited. The new cap is
// applied on the next Add; it does not evict sessions that are already stored.
func (m *manager) SetMaxSessions(n int) {
	m.mu.Lock()
	m.maxSessions = n
	m.mu.Unlock()
}

func (m *manager) Remove(sess *Session, w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.opts.CookieName,
		Value:    "",
		Path:     m.opts.CookiePath,
		HttpOnly: true,
		Secure:   !m.opts.AllowHTTP,
		MaxAge:   -1,
	})
	m.mu.Lock()
	delete(m.sessions, sess.ID())
	m.mu.Unlock()
}

func (m *manager) Close() {
	m.closeOnce.Do(func() { close(m.done) })
}

func (m *manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// -------------------------------------------------------------------------
// Package-level API
// -------------------------------------------------------------------------

// global is swapped atomically because Init may run while other goroutines are
// serving requests through Get/Add/Remove.
var global atomic.Pointer[manager]

func defaultOptions() Options {
	return Options{AllowHTTP: true, CleanInterval: 30 * time.Minute}
}

func init() {
	global.Store(newManager(defaultOptions()))
}

// Init closes the current global manager and replaces it with a new one
// configured by o. Use this at startup or in tests to override defaults.
//
// Init discards every stored session. Call it once, at startup: it is not a
// way to reconfigure a running server.
func Init(o Options) {
	old := global.Swap(newManager(o))
	old.Close()
}

func Reinit() { Init(defaultOptions()) }

// Get returns the session associated with r, or nil if none exists.
func Get(r *http.Request) *Session { return global.Load().Get(r) }

// Add stores sess and sets the session cookie on w.
func Add(sess *Session, w http.ResponseWriter) { global.Load().Add(sess, w) }

// Remove deletes sess and instructs the client to clear its cookie.
func Remove(sess *Session, w http.ResponseWriter) { global.Load().Remove(sess, w) }

// Close stops the background cleaner goroutine.
func Close() { global.Load().Close() }

// Len returns the number of active sessions in the global manager.
func Len() int { return global.Load().Len() }

// SetMaxSessions changes the global manager's session cap. 0 means unlimited.
func SetMaxSessions(n int) { global.Load().SetMaxSessions(n) }
