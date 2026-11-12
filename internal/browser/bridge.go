// Package browser implements `dev browser`: the MCP bridge that exposes
// chrome-devtools-mcp (a stdio MCP server) over Streamable HTTP, the launchd
// jobs that keep the bridge and its reverse SSH tunnel alive, port picking,
// and MCP registration with the agents inside the devcontainer.
package browser

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrChildExited is returned by Bridge when the MCP child process goes away.
// The launchd job has KeepAlive, so exiting non-zero gets a fresh bridge and
// child.
var ErrChildExited = errors.New("mcp child process exited")

const sessionIDHeader = "Mcp-Session-Id"

// Bridge serves Streamable HTTP at http://<listenAddr>/mcp and forwards every
// MCP session to one shared child process speaking MCP over stdio.
//
// It is a JSON-RPC level proxy, not an MCP server of its own: tools,
// resources, prompts, notifications and server-to-client requests pass
// through unchanged, except that request IDs are remapped so several HTTP
// sessions can share the child (Chrome asks the user to approve each new
// debugging session, so one child means one approval). The child is
// initialized once, with the first client's initialize request; later
// clients get the cached result.
//
// Bridge returns nil when ctx is cancelled, and ErrChildExited (wrapped) when
// the child dies, so the caller can exit non-zero and let launchd restart it.
func Bridge(ctx context.Context, listenAddr string, command []string, instructions string) error {
	if len(command) == 0 {
		return errors.New("bridge: no command given")
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	child, err := (&mcp.CommandTransport{Command: cmd}).Connect(ctx)
	if err != nil {
		ln.Close()
		return fmt.Errorf("start %s: %w", command[0], err)
	}
	logger := log.New(os.Stderr, "bridge: ", log.LstdFlags)
	logger.Printf("listening on http://%s/mcp, child %q (pid %d)", ln.Addr(), strings.Join(command, " "), cmd.Process.Pid)
	return serveBridge(ctx, ln, child, logger, instructions)
}

// serveBridge runs the proxy on ln with an already connected child.
func serveBridge(ctx context.Context, ln net.Listener, child mcp.Connection, logger *log.Logger, instructions string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := newProxy(child, logger)
	b.instructions = instructions

	mux := http.NewServeMux()
	mux.Handle("/mcp", b)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- fmt.Errorf("%w: %v", ErrChildExited, b.readChild(ctx)) }()
	go func() { errc <- srv.Serve(ln) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	cancel()
	shutCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	b.closeAll()
	_ = srv.Shutdown(shutCtx)
	_ = child.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// proxy holds the shared child connection and all HTTP sessions.
type proxy struct {
	child  mcp.Connection
	logger *log.Logger
	wmu    sync.Mutex // serializes writes to the child

	mu       sync.Mutex
	sessions map[string]*session
	nextID   int64
	// pending maps a child-side request ID to the client request it carries.
	pending map[int64]*pendingCall
	// progress maps a progress token to the session that owns it.
	progress map[string]*session
	// fromChild maps a bridge-assigned ID (sent to a client) to the child's
	// original ID, for server-to-client requests.
	fromChild  map[string]childReq
	lastActive *session

	// instructions are added to the child's initialize result (MCP server
	// instructions, which clients put in the model's context).
	instructions string

	// Initialization of the child.
	initState   int // 0 not started, 1 in flight, 2 done
	initResult  json.RawMessage
	initWaiters []waiter
}

type pendingCall struct {
	sess   *session
	origID jsonrpc.ID
	token  string // progress token key, if any
	init   bool
}

type childReq struct {
	sess    *session
	childID jsonrpc.ID
}

type waiter struct {
	sess *session
	id   jsonrpc.ID
}

type session struct {
	id        string
	transport *mcp.StreamableServerTransport
	conn      mcp.Connection
	cancel    context.CancelFunc

	// guarded by proxy.mu
	initialized bool
	closed      bool
}

func newProxy(child mcp.Connection, logger *log.Logger) *proxy {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &proxy{
		child:     child,
		logger:    logger,
		sessions:  map[string]*session{},
		pending:   map[int64]*pendingCall{},
		progress:  map[string]*session{},
		fromChild: map[string]childReq{},
	}
}

// ServeHTTP implements the Streamable HTTP session management that the SDK's
// StreamableHTTPHandler does for an mcp.Server, but hands each session's
// connection to the proxy instead.
func (b *proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// DNS rebinding protection, as in the SDK: a loopback listener only
	// answers loopback Host headers.
	if la, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && la != nil {
		if isLoopback(la.String()) && !isLoopback(req.Host) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", req.Host), http.StatusForbidden)
			return
		}
	}
	if req.Body != nil {
		req.Body = http.MaxBytesReader(w, req.Body, mcp.DefaultMaxRequestBodyBytes)
	}

	sid := req.Header.Get(sessionIDHeader)
	if sid != "" {
		b.mu.Lock()
		s := b.sessions[sid]
		b.mu.Unlock()
		if s == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		switch req.Method {
		case http.MethodGet, http.MethodPost:
			s.transport.ServeHTTP(w, req)
		case http.MethodDelete:
			b.closeSession(s)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch req.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		http.Error(w, "Bad Request: missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// A POST without a session: answer server/discover (SEP-2575 stateless
	// probe) directly so the client falls back to initialize; otherwise
	// open a new session.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if msg, err := jsonrpc.DecodeMessage(body); err == nil {
		if r, ok := msg.(*jsonrpc.Request); ok && r.Method == "server/discover" && r.IsCall() {
			resp := &jsonrpc.Response{ID: r.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "server/discover not supported; use initialize"}}
			data, _ := jsonrpc.EncodeMessage(resp)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
	}

	s, err := b.newSession()
	if err != nil {
		http.Error(w, "failed connection", http.StatusInternalServerError)
		return
	}
	s.transport.ServeHTTP(w, req)
	// A session whose first POST was not a successful initialize is useless
	// (the client never learned its ID); drop it, as the SDK does.
	b.mu.Lock()
	ok := s.initialized
	b.mu.Unlock()
	if !ok {
		b.closeSession(s)
	}
}

func isLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (b *proxy) newSession() (*session, error) {
	t := &mcp.StreamableServerTransport{SessionID: crand.Text()}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := t.Connect(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &session{id: t.SessionID, transport: t, conn: conn, cancel: cancel}
	b.mu.Lock()
	b.sessions[s.id] = s
	b.mu.Unlock()
	go b.readSession(ctx, s)
	return s, nil
}

// closeSession forgets s, cancels its in-flight requests at the child and
// fails the child's requests that were waiting on s.
func (b *proxy) closeSession(s *session) {
	b.mu.Lock()
	if s.closed {
		b.mu.Unlock()
		return
	}
	s.closed = true
	delete(b.sessions, s.id)
	if b.lastActive == s {
		b.lastActive = nil
	}
	var cancelIDs []int64
	for cid, pc := range b.pending {
		if pc.sess == s && !pc.init {
			cancelIDs = append(cancelIDs, cid)
			delete(b.pending, cid)
			if pc.token != "" {
				delete(b.progress, pc.token)
			}
		}
	}
	var orphaned []jsonrpc.ID
	for bid, cr := range b.fromChild {
		if cr.sess == s {
			orphaned = append(orphaned, cr.childID)
			delete(b.fromChild, bid)
		}
	}
	b.mu.Unlock()

	s.cancel()
	_ = s.conn.Close()
	for _, cid := range cancelIDs {
		n, _ := notification("notifications/cancelled", map[string]any{"requestId": cid, "reason": "client session closed"})
		b.writeChild(n)
	}
	for _, id := range orphaned {
		b.writeChild(&jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "client session closed"}})
	}
}

func (b *proxy) closeAll() {
	b.mu.Lock()
	all := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		all = append(all, s)
	}
	b.mu.Unlock()
	for _, s := range all {
		b.closeSession(s)
	}
}

func (b *proxy) writeChild(msg jsonrpc.Message) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if err := b.child.Write(context.Background(), msg); err != nil {
		b.logger.Printf("write to child: %v", err)
	}
}

func (b *proxy) writeSession(s *session, msg jsonrpc.Message) {
	if err := s.conn.Write(context.Background(), msg); err != nil {
		b.logger.Printf("write to session %s: %v", short(s.id), err)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func respond(id jsonrpc.ID, result any) *jsonrpc.Response {
	raw, _ := json.Marshal(result)
	return &jsonrpc.Response{ID: id, Result: raw}
}

// readSession pumps messages from one HTTP session to the child.
func (b *proxy) readSession(ctx context.Context, s *session) {
	defer b.closeSession(s)
	for {
		msg, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *jsonrpc.Request:
			if m.IsCall() {
				b.clientCall(s, m)
			} else {
				b.clientNotification(s, m)
			}
		case *jsonrpc.Response:
			b.clientResponse(s, m)
		}
	}
}

func (b *proxy) clientCall(s *session, m *jsonrpc.Request) {
	switch m.Method {
	case "initialize":
		b.initialize(s, m)
		return
	case "ping":
		b.writeSession(s, respond(m.ID, struct{}{}))
		return
	}
	b.mu.Lock()
	if s.closed {
		b.mu.Unlock()
		return
	}
	b.nextID++
	cid := b.nextID
	pc := &pendingCall{sess: s, origID: m.ID}
	if tok, ok := progressToken(m.Params); ok {
		pc.token = tok
		b.progress[tok] = s
	}
	b.pending[cid] = pc
	b.lastActive = s
	b.mu.Unlock()
	b.writeChild(&jsonrpc.Request{ID: intID(cid), Method: m.Method, Params: m.Params})
}

func (b *proxy) initialize(s *session, m *jsonrpc.Request) {
	b.mu.Lock()
	switch b.initState {
	case 2:
		s.initialized = true
		res := b.initResult
		b.mu.Unlock()
		b.writeSession(s, &jsonrpc.Response{ID: m.ID, Result: res})
		return
	case 1:
		b.initWaiters = append(b.initWaiters, waiter{s, m.ID})
		b.mu.Unlock()
		return
	}
	b.initState = 1
	b.initWaiters = append(b.initWaiters, waiter{s, m.ID})
	b.nextID++
	cid := b.nextID
	b.pending[cid] = &pendingCall{sess: s, origID: m.ID, init: true}
	b.mu.Unlock()
	b.writeChild(&jsonrpc.Request{ID: intID(cid), Method: m.Method, Params: m.Params})
}

func (b *proxy) clientNotification(s *session, m *jsonrpc.Request) {
	switch m.Method {
	case "notifications/initialized":
		// The bridge sends this to the child itself, once.
		return
	case "notifications/cancelled":
		var p struct {
			RequestID any    `json:"requestId"`
			Reason    string `json:"reason,omitempty"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		orig, err := jsonrpc.MakeID(p.RequestID)
		if err != nil {
			return
		}
		b.mu.Lock()
		var cid int64
		for id, pc := range b.pending {
			if pc.sess == s && pc.origID == orig && !pc.init {
				cid = id
				break
			}
		}
		b.mu.Unlock()
		if cid == 0 {
			return
		}
		n, _ := notification(m.Method, map[string]any{"requestId": cid, "reason": p.Reason})
		b.writeChild(n)
		return
	}
	b.writeChild(m)
}

func (b *proxy) clientResponse(s *session, m *jsonrpc.Response) {
	bid, _ := m.ID.Raw().(string)
	b.mu.Lock()
	cr, ok := b.fromChild[bid]
	if ok && cr.sess == s {
		delete(b.fromChild, bid)
	}
	b.mu.Unlock()
	if !ok || cr.sess != s {
		return
	}
	b.writeChild(&jsonrpc.Response{ID: cr.childID, Result: m.Result, Error: m.Error})
}

// readChild pumps messages from the child to the sessions until the child
// connection fails.
func (b *proxy) readChild(ctx context.Context) error {
	for {
		msg, err := b.child.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch m := msg.(type) {
		case *jsonrpc.Response:
			b.childResponse(m)
		case *jsonrpc.Request:
			if m.IsCall() {
				b.childCall(m)
			} else {
				b.childNotification(m)
			}
		}
	}
}

func (b *proxy) childResponse(m *jsonrpc.Response) {
	cid, ok := m.ID.Raw().(int64)
	if !ok {
		return
	}
	b.mu.Lock()
	pc := b.pending[cid]
	delete(b.pending, cid)
	if pc != nil && pc.token != "" {
		delete(b.progress, pc.token)
	}
	if pc == nil {
		b.mu.Unlock()
		return
	}
	if pc.init {
		waiters := b.initWaiters
		b.initWaiters = nil
		if m.Error == nil {
			m.Result = withInstructions(m.Result, b.instructions)
			b.initState = 2
			b.initResult = m.Result
			for _, w := range waiters {
				w.sess.initialized = true
			}
		} else {
			b.initState = 0
		}
		b.mu.Unlock()
		if m.Error == nil {
			n, _ := notification("notifications/initialized", map[string]any{})
			b.writeChild(n)
		}
		for _, w := range waiters {
			b.writeSession(w.sess, &jsonrpc.Response{ID: w.id, Result: m.Result, Error: m.Error})
		}
		return
	}
	b.mu.Unlock()
	b.writeSession(pc.sess, &jsonrpc.Response{ID: pc.origID, Result: m.Result, Error: m.Error})
}

// childCall routes a server-to-client request (roots/list, sampling,
// elicitation) to the session that most recently sent a request.
func (b *proxy) childCall(m *jsonrpc.Request) {
	if m.Method == "ping" {
		b.writeChild(respond(m.ID, struct{}{}))
		return
	}
	b.mu.Lock()
	target := b.lastActive
	if target == nil || target.closed || !target.initialized {
		target = nil
		for _, s := range b.sessions {
			if s.initialized {
				target = s
				break
			}
		}
	}
	if target == nil {
		b.mu.Unlock()
		b.writeChild(&jsonrpc.Response{ID: m.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "no MCP client connected"}})
		return
	}
	b.nextID++
	bid := fmt.Sprintf("dev-bridge-%d", b.nextID)
	b.fromChild[bid] = childReq{sess: target, childID: m.ID}
	b.mu.Unlock()
	b.writeSession(target, &jsonrpc.Request{ID: strID(bid), Method: m.Method, Params: m.Params})
}

func (b *proxy) childNotification(m *jsonrpc.Request) {
	switch m.Method {
	case "notifications/progress":
		tok, _ := tokenKey(m.Params, "progressToken")
		b.mu.Lock()
		s := b.progress[tok]
		b.mu.Unlock()
		if s != nil {
			b.writeSession(s, m)
		}
		return
	case "notifications/cancelled":
		// The child cancels one of its own requests to a client.
		var p map[string]any
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		cid, err := jsonrpc.MakeID(p["requestId"])
		if err != nil {
			return
		}
		b.mu.Lock()
		var target *session
		for bid, cr := range b.fromChild {
			if cr.childID == cid {
				target = cr.sess
				p["requestId"] = bid
				delete(b.fromChild, bid)
				break
			}
		}
		b.mu.Unlock()
		if target != nil {
			n, _ := notification(m.Method, p)
			b.writeSession(target, n)
		}
		return
	}
	// list_changed, logging, resource updates: every initialized session.
	b.mu.Lock()
	var targets []*session
	for _, s := range b.sessions {
		if s.initialized {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()
	for _, s := range targets {
		b.writeSession(s, m)
	}
}

// progressToken extracts params._meta.progressToken as a map key.
func progressToken(params json.RawMessage) (string, bool) {
	var p struct {
		Meta json.RawMessage `json:"_meta"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil || len(p.Meta) == 0 {
		return "", false
	}
	return tokenKey(p.Meta, "progressToken")
}

func tokenKey(obj json.RawMessage, field string) (string, bool) {
	var m map[string]any
	if json.Unmarshal(obj, &m) != nil {
		return "", false
	}
	v, ok := m[field]
	if !ok || v == nil {
		return "", false
	}
	return fmt.Sprintf("%T:%v", v, v), true
}

func intID(n int64) jsonrpc.ID {
	id, _ := jsonrpc.MakeID(float64(n))
	return id
}

func strID(s string) jsonrpc.ID {
	id, _ := jsonrpc.MakeID(s)
	return id
}

func notification(method string, params any) (*jsonrpc.Request, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &jsonrpc.Request{Method: method, Params: raw}, nil
}

// withInstructions puts extra in the initialize result's "instructions",
// ahead of any instructions the child sent. On a result it can't parse it
// returns the result unchanged.
func withInstructions(result json.RawMessage, extra string) json.RawMessage {
	if extra == "" {
		return result
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil || m == nil {
		return result
	}
	text := extra
	var theirs string
	if raw, ok := m["instructions"]; ok && json.Unmarshal(raw, &theirs) == nil && strings.TrimSpace(theirs) != "" {
		text = extra + "\n\n" + theirs
	}
	b, err := json.Marshal(text)
	if err != nil {
		return result
	}
	m["instructions"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return result
	}
	return out
}

// Instructions tells agents in the container that the browser runs on the
// user's Mac, so localhost there is the Mac, and how to reach this project's
// services instead (its Tailscale name).
func Instructions(host string) string {
	s := "This browser is the user's real Chrome running on their Mac (the host machine), " +
		"not inside this container or VM. The browser resolves every URL on the Mac: " +
		"localhost and 127.0.0.1 in the browser mean the Mac, not this environment, " +
		"so dev servers and docker compose services running here are NOT reachable at localhost in the browser."
	if host != "" {
		s += " To open a service listening on port N here, use http://" + host + ":N " +
			"(this environment's Tailscale name), e.g. http://" + host + ":8080."
	}
	s += " Your own shell tools here (curl, test runners) still reach those services at localhost:N. " +
		"This is the user's own browser profile with their logins: don't change settings, " +
		"submit forms or make purchases unless asked."
	return s
}
