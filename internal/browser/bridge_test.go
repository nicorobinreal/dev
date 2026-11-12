package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeChildEnv = "DEV_BROWSER_FAKE_MCP_CHILD"

// TestMain re-execs the test binary as a tiny stdio MCP server when
// fakeChildEnv is set.
func TestMain(m *testing.M) {
	if os.Getenv(fakeChildEnv) == "1" {
		runFakeChild()
		return
	}
	os.Exit(m.Run())
}

type echoArgs struct {
	Text string `json:"text"`
}

func runFakeChild() {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-chrome", Version: "0.0.1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo text with pid"},
		func(ctx context.Context, req *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%s pid=%d", a.Text, os.Getpid())}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "roots", Description: "ask the client for its roots"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			res, err := req.Session.ListRoots(ctx, nil) //nolint:staticcheck // exercises server-to-client request routing
			if err != nil {
				return nil, nil, err
			}
			var uris []string
			for _, r := range res.Roots {
				uris = append(uris, r.URI)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(uris, ",")}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "progress", Description: "report progress"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			tok := req.Params.GetProgressToken()
			if tok != nil {
				if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: 1, Total: 2, Message: "half"}); err != nil {
					return nil, nil, err
				}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "crash", Description: "exit the process"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			os.Exit(0)
			return nil, nil, nil
		})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake child:", err)
		os.Exit(1)
	}
}

// startBridge runs the bridge with a fake child and returns its /mcp URL and
// a channel with serveBridge's result.
func startBridge(t *testing.T) (string, chan error, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeChildEnv+"=1")
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithCancel(context.Background())
	child, err := (&mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveBridge(ctx, ln, child, log.New(os.Stderr, "bridge: ", 0)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return "http://" + ln.Addr().String() + "/mcp", done, cancel
}

func connect(t *testing.T, ctx context.Context, url string, opts *mcp.ClientOptions, root string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, opts)
	if root != "" {
		c.AddRoots(&mcp.Root{URI: root}) //nolint:staticcheck // see ListRoots in runFakeChild
	}
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content: %+v", res.Content)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestBridgeEndToEnd(t *testing.T) {
	url, _, _ := startBridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var progMu sync.Mutex
	var progress []string
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progMu.Lock()
			progress = append(progress, req.Params.Message)
			progMu.Unlock()
		},
	}
	a := connect(t, ctx, url, opts, "file:///a")

	if a.InitializeResult().ServerInfo.Name != "fake-chrome" {
		t.Errorf("server info = %+v, want the child's", a.InitializeResult().ServerInfo)
	}
	tools, err := a.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "echo") || !strings.Contains(got, "roots") {
		t.Errorf("tools = %s", got)
	}

	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	gotA := text(t, res)
	if !strings.HasPrefix(gotA, "hi pid=") {
		t.Fatalf("echo = %q", gotA)
	}

	// A second session shares the same child process (one Chrome approval).
	b := connect(t, ctx, url, nil, "file:///b")
	res, err = b.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotB := text(t, res); gotB != gotA {
		t.Errorf("second session got %q, want same child %q", gotB, gotA)
	}

	// Concurrent calls from both sessions keep their IDs straight.
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, cs := range []*mcp.ClientSession{a, b} {
			wg.Add(1)
			go func(cs *mcp.ClientSession, i int) {
				defer wg.Done()
				want := fmt.Sprintf("m%d", i)
				r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": want}})
				if err != nil {
					errs <- err
					return
				}
				if got := r.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(got, want+" ") {
					errs <- fmt.Errorf("got %q want prefix %q", got, want)
				}
			}(cs, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Server-to-client requests reach the session that made the call.
	res, err = b.CallTool(ctx, &mcp.CallToolParams{Name: "roots"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "file:///b" {
		t.Errorf("roots via b = %q", got)
	}
	res, err = a.CallTool(ctx, &mcp.CallToolParams{Name: "roots"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "file:///a" {
		t.Errorf("roots via a = %q", got)
	}

	// Progress notifications are routed by token.
	params := &mcp.CallToolParams{Name: "progress"}
	params.SetProgressToken("tok-1")
	res, err = a.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if text(t, res) != "done" {
		t.Error("progress tool result")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		progMu.Lock()
		n := len(progress)
		progMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	progMu.Lock()
	if len(progress) != 1 || progress[0] != "half" {
		t.Errorf("progress = %v", progress)
	}
	progMu.Unlock()

	// Closing one session leaves the other working.
	b.Close()
	if _, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "still"}}); err != nil {
		t.Errorf("after closing b: %v", err)
	}
}

func TestBridgeLegacyProtocolClient(t *testing.T) {
	url, _, _ := startBridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "old", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "old"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text(t, res), "old pid=") {
		t.Error(text(t, res))
	}
}

func TestBridgeRejectsForeignHost(t *testing.T) {
	url, _, _ := startBridge(t)
	req, _ := newPost(url, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	req.Host = "evil.example.com"
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}

	req, _ = newPost(url, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req.Header.Set(sessionIDHeader, "nope")
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown session status = %d, want 404", resp.StatusCode)
	}
}

func TestBridgeChildExit(t *testing.T) {
	url, done, _ := startBridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs := connect(t, ctx, url, nil, "")
	go cs.CallTool(ctx, &mcp.CallToolParams{Name: "crash"})
	select {
	case err := <-done:
		if !errors.Is(err, ErrChildExited) {
			t.Errorf("err = %v, want ErrChildExited", err)
		}
		done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("bridge kept running after child exit")
	}
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

func newPost(url, body string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req, nil
}
