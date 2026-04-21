package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newUpstream(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func newGateServer(upstreamURL string) (*Gate, *httptest.Server) {
	gate := NewGate(upstreamURL)
	srv := httptest.NewServer(gate)
	return gate, srv
}

// sendAsync fires an HTTP POST in a goroutine and returns the response channel.
func sendAsync(t *testing.T, gateURL, path, body, authHeader string) <-chan *http.Response {
	t.Helper()
	ch := make(chan *http.Response, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, gateURL+path, strings.NewReader(body))
		if err != nil {
			t.Errorf("build request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("do request: %v", err)
			return
		}
		ch <- resp
	}()
	return ch
}

// waitForQueue polls until the gate holds exactly n requests or times out.
func waitForQueue(t *testing.T, gate *Gate, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gate.QueueLen() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for queue length %d (got %d)", n, gate.QueueLen())
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// drainResponses collects all responses from channels with a timeout.
func drainResponses(t *testing.T, channels []<-chan *http.Response, timeout time.Duration) []*http.Response {
	t.Helper()
	responses := make([]*http.Response, 0, len(channels))
	for i, ch := range channels {
		select {
		case resp := <-ch:
			responses = append(responses, resp)
		case <-time.After(timeout):
			t.Errorf("request %d timed out waiting for response", i)
		}
	}
	return responses
}

// ── tests ─────────────────────────────────────────────────────────────────────

// Test: Requests are held and NOT forwarded to upstream until ReleaseAll is called
func TestRequestsAreHeldUntilRelease(t *testing.T) {
	upstreamHit := make(chan struct{}, 10)
	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	// NOTE: always ReleaseAll before Close — parked goroutines hold open TCP
	// connections and httptest.Server.Close() will block waiting for them.
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh1 := sendAsync(t, gateSrv.URL, "/api/transfer", `{"amount":100}`, "")
	respCh2 := sendAsync(t, gateSrv.URL, "/api/transfer", `{"amount":100}`, "")

	waitForQueue(t, gate, 2, 2*time.Second)

	// Upstream must NOT have been hit yet
	select {
	case <-upstreamHit:
		t.Fatal("upstream was hit before ReleaseAll — requests were not held")
	default:
		// correct: nothing forwarded yet
	}

	// Now release so deferred Close doesn't hang
	gate.ReleaseAll()

	drainResponses(t, []<-chan *http.Response{respCh1, respCh2}, 3*time.Second)
}

// Test: ReleaseAll forwards all held requests and returns the correct count
func TestReleaseAllForwardsAllRequests(t *testing.T) {
	const requestCount = 5

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	channels := make([]<-chan *http.Response, requestCount)
	for i := range channels {
		channels[i] = sendAsync(t, gateSrv.URL, "/api/transfer", `{"amount":100}`, "")
	}

	waitForQueue(t, gate, requestCount, 2*time.Second)

	released := gate.ReleaseAll()

	if released != requestCount {
		t.Errorf("ReleaseAll returned %d, want %d", released, requestCount)
	}

	responses := drainResponses(t, channels, 3*time.Second)
	for i, resp := range responses {
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// Test: All requests arrive at upstream within a narrow time window (< 50ms)
func TestRequestsAreReleasedSimultaneously(t *testing.T) {
	const requestCount = 8
	const maxSpreadMs = 50

	var (
		mu           sync.Mutex
		arrivalTimes []time.Time
	)

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivalTimes = append(arrivalTimes, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	channels := make([]<-chan *http.Response, requestCount)
	for i := range channels {
		channels[i] = sendAsync(t, gateSrv.URL, "/api/transfer", `{"amount":100}`, "")
	}
	waitForQueue(t, gate, requestCount, 2*time.Second)

	gate.ReleaseAll()
	drainResponses(t, channels, 3*time.Second)

	if len(arrivalTimes) != requestCount {
		t.Fatalf("upstream received %d requests, want %d", len(arrivalTimes), requestCount)
	}

	earliest, latest := arrivalTimes[0], arrivalTimes[0]
	for _, ts := range arrivalTimes[1:] {
		if ts.Before(earliest) {
			earliest = ts
		}
		if ts.After(latest) {
			latest = ts
		}
	}
	spread := latest.Sub(earliest)
	if spread > maxSpreadMs*time.Millisecond {
		t.Errorf("arrival spread %v, want < %dms", spread, maxSpreadMs)
	}
}

// Test: ReleaseAll on an empty queue returns 0 and does not panic
func TestReleaseAllOnEmptyQueue(t *testing.T) {
	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()

	released := gate.ReleaseAll()

	if released != 0 {
		t.Errorf("expected 0 from empty ReleaseAll, got %d", released)
	}
}

// Test: ReleaseAll works correctly across multiple sequential rounds
func TestMultipleReleaseAllRounds(t *testing.T) {
	var totalHits atomic.Int32

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		totalHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	// Round 1: 3 requests
	round1 := make([]<-chan *http.Response, 3)
	for i := range round1 {
		round1[i] = sendAsync(t, gateSrv.URL, "/api/op", `{}`, "")
	}
	waitForQueue(t, gate, 3, 2*time.Second)
	gate.ReleaseAll()
	drainResponses(t, round1, 3*time.Second)

	// Round 2: 2 requests
	round2 := make([]<-chan *http.Response, 2)
	for i := range round2 {
		round2[i] = sendAsync(t, gateSrv.URL, "/api/op", `{}`, "")
	}
	waitForQueue(t, gate, 2, 2*time.Second)
	gate.ReleaseAll()
	drainResponses(t, round2, 3*time.Second)

	if int(totalHits.Load()) != 5 {
		t.Errorf("upstream received %d total hits, want 5", totalHits.Load())
	}
}

// Test: Request body bytes arrive at upstream unchanged
func TestRequestBodyIsForwarded(t *testing.T) {
	const payload = `{"userId":"user-123","amount":500}`
	receivedBody := make(chan string, 1)

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody <- string(b)
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/transfer", payload, "")
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()
	drainResponses(t, []<-chan *http.Response{respCh}, 3*time.Second)

	select {
	case body := <-receivedBody:
		if body != payload {
			t.Errorf("upstream body = %q, want %q", body, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream body")
	}
}

// Test: Authorization and other headers are forwarded to upstream
func TestRequestHeadersAreForwarded(t *testing.T) {
	const token = "Bearer test-token-xyz"
	receivedAuth := make(chan string, 1)

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/transfer", `{}`, token)
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()
	drainResponses(t, []<-chan *http.Response{respCh}, 3*time.Second)

	select {
	case auth := <-receivedAuth:
		if auth != token {
			t.Errorf("Authorization = %q, want %q", auth, token)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for header")
	}
}

// Test: Upstream response body and headers are returned to the original caller
func TestUpstreamResponseIsReturnedToCaller(t *testing.T) {
	const upstreamBody = `{"status":"ok","balance":900}`

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/balance", `{}`, "")
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()

	responses := drainResponses(t, []<-chan *http.Response{respCh}, 3*time.Second)
	if len(responses) == 0 {
		t.Fatal("no response received")
	}

	body := readBody(t, responses[0])
	if body != upstreamBody {
		t.Errorf("response body = %q, want %q", body, upstreamBody)
	}
	ct := responses[0].Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// Test: Non-200 upstream status codes (e.g. 409 Conflict) are returned unchanged
func TestUpstreamErrorStatusIsForwardedToCaller(t *testing.T) {
	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"duplicate transaction"}`)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/transfer", `{}`, "")
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()

	responses := drainResponses(t, []<-chan *http.Response{respCh}, 3*time.Second)
	if len(responses) == 0 {
		t.Fatal("no response received")
	}
	defer responses[0].Body.Close()

	if responses[0].StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", responses[0].StatusCode)
	}
}

// Test: Unreachable upstream returns 502 Bad Gateway to the caller
func TestUpstreamUnreachableReturns502(t *testing.T) {
	gate, gateSrv := newGateServer("http://127.0.0.1:19999") // nothing listening
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/transfer", `{}`, "")
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()

	responses := drainResponses(t, []<-chan *http.Response{respCh}, 5*time.Second)
	if len(responses) == 0 {
		t.Fatal("no response received")
	}
	defer responses[0].Body.Close()

	if responses[0].StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", responses[0].StatusCode)
	}
}

// Test: QueueLen accurately reflects held request count before and after release
func TestQueueLenAccuracy(t *testing.T) {
	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	if gate.QueueLen() != 0 {
		t.Errorf("fresh gate QueueLen = %d, want 0", gate.QueueLen())
	}

	channels := make([]<-chan *http.Response, 3)
	for i := range channels {
		channels[i] = sendAsync(t, gateSrv.URL, "/api/transfer", `{}`, "")
	}
	waitForQueue(t, gate, 3, 2*time.Second)

	if gate.QueueLen() != 3 {
		t.Errorf("QueueLen = %d, want 3", gate.QueueLen())
	}

	gate.ReleaseAll()
	drainResponses(t, channels, 3*time.Second)

	// Allow the queue to be cleared
	time.Sleep(100 * time.Millisecond)
	if gate.QueueLen() != 0 {
		t.Errorf("QueueLen after release = %d, want 0", gate.QueueLen())
	}
}

// Test: GET, PUT, DELETE methods are all proxied correctly
func TestDifferentHTTPMethodsAreProxied(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			receivedMethod := make(chan string, 1)

			upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
				receivedMethod <- r.Method
				w.WriteHeader(http.StatusOK)
			})
			defer upstream.Close()

			gate := NewGate(upstream.URL)
			gateSrv := httptest.NewServer(gate)
			defer gateSrv.Close()
			defer gate.ReleaseAll()

			respCh := make(chan *http.Response, 1)
			go func() {
				req, _ := http.NewRequest(method, gateSrv.URL+"/api/resource", nil)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					respCh <- resp
				}
			}()

			waitForQueue(t, gate, 1, 2*time.Second)
			gate.ReleaseAll()

			// Drain response to unblock connection
			select {
			case resp := <-respCh:
				resp.Body.Close()
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for response")
			}

			select {
			case m := <-receivedMethod:
				if m != method {
					t.Errorf("upstream method = %q, want %q", m, method)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for upstream to receive %s", method)
			}
		})
	}
}

// Test: URL path and query string are preserved through the gate
func TestURLPathAndQueryArePreserved(t *testing.T) {
	receivedURI := make(chan string, 1)

	upstream := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		receivedURI <- r.RequestURI
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	gate, gateSrv := newGateServer(upstream.URL)
	defer gateSrv.Close()
	defer gate.ReleaseAll()

	respCh := sendAsync(t, gateSrv.URL, "/api/transfer?dry_run=true&trace=1", `{}`, "")
	waitForQueue(t, gate, 1, 2*time.Second)
	gate.ReleaseAll()
	drainResponses(t, []<-chan *http.Response{respCh}, 3*time.Second)

	select {
	case uri := <-receivedURI:
		if uri != "/api/transfer?dry_run=true&trace=1" {
			t.Errorf("upstream URI = %q, want /api/transfer?dry_run=true&trace=1", uri)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream URI")
	}
}
