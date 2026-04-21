package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// HeldRequest stores an incoming request parked at the gate
type HeldRequest struct {
	w         http.ResponseWriter
	r         *http.Request
	body      []byte
	arrivedAt time.Time
	done      chan struct{} // closed when proxy response is written
}

// Gate holds incoming requests and releases them simultaneously on demand.
type Gate struct {
	targetURL string

	mu      sync.Mutex
	queue   []*HeldRequest
	queueCh chan *HeldRequest
}

func NewGate(targetURL string) *Gate {
	g := &Gate{
		targetURL: targetURL,
		queueCh:   make(chan *HeldRequest, 256),
	}
	go g.drainQueueCh()
	return g
}

// drainQueueCh moves arriving requests from the channel into the queue slice.
func (g *Gate) drainQueueCh() {
	for req := range g.queueCh {
		g.mu.Lock()
		g.queue = append(g.queue, req)
		n := len(g.queue)
		g.mu.Unlock()
		log.Printf("[HELD  #%02d] %s %s  (arrived %s)",
			n, req.r.Method, req.r.URL.RequestURI(),
			req.arrivedAt.Format("15:04:05.000"))
	}
}

// ServeHTTP implements http.Handler — parks each request until ReleaseAll is called.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	held := &HeldRequest{
		w:         w,
		r:         r,
		body:      body,
		arrivedAt: time.Now(),
		done:      make(chan struct{}),
	}
	g.queueCh <- held
	<-held.done // block until ReleaseAll closes this channel
}

// QueueLen returns the number of currently held requests.
func (g *Gate) QueueLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue)
}

// ReleaseAll fires all held requests simultaneously and waits for all to complete.
// Returns the number of requests released.
func (g *Gate) ReleaseAll() int {
	g.mu.Lock()
	batch := make([]*HeldRequest, len(g.queue))
	copy(batch, g.queue)
	g.queue = nil
	g.mu.Unlock()

	if len(batch) == 0 {
		return 0
	}

	log.Printf("[GATE] Releasing %d request(s) simultaneously...", len(batch))
	firedAt := time.Now()

	startGun := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(batch))

	for i, held := range batch {
		go func(idx int, h *HeldRequest) {
			defer wg.Done()
			<-startGun // park until gun fires

			holdDuration := time.Since(h.arrivedAt)
			log.Printf("[FIRE  #%02d] forwarding (held %v)", idx+1, holdDuration.Round(time.Millisecond))

			g.forward(h)
			close(h.done) // unblock the original handler goroutine
		}(i+1, held)
	}

	close(startGun) // broadcast: unblocks all goroutines in the same scheduler pass
	wg.Wait()

	log.Printf("[GATE] All %d requests completed in %v.",
		len(batch), time.Since(firedAt).Round(time.Microsecond))
	return len(batch)
}

// forward proxies a single held request to the target and writes the response back.
func (g *Gate) forward(h *HeldRequest) {
	url := g.targetURL + h.r.RequestURI

	req, err := http.NewRequestWithContext(h.r.Context(), h.r.Method, url, bytes.NewReader(h.body))
	if err != nil {
		http.Error(h.w, fmt.Sprintf("proxy build error: %v", err), http.StatusBadGateway)
		return
	}

	for key, vals := range h.r.Header {
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(h.w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		log.Printf("[ERROR] %s %s → %v", h.r.Method, h.r.RequestURI, err)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			h.w.Header().Add(key, v)
		}
	}
	h.w.WriteHeader(resp.StatusCode)
	io.Copy(h.w, resp.Body)

	log.Printf("[DONE ] %s %s → HTTP %d", h.r.Method, h.r.RequestURI, resp.StatusCode)
}
