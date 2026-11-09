package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newMockExporter(up bool, recovery bool) *Exporter {
	s := &Server{
		Database:  "postgres",
		Databases: map[string]bool{"postgres": true},
		UP:        up,
		Recovery:  recovery,
	}
	s.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = up
		s.Recovery = recovery
		return nil
	}
	return &Exporter{server: s}
}

func TestReloadAndHealthHandlersNoDeadlock(t *testing.T) {
	originalExporter := PgExporter
	defer setCurrentExporter(originalExporter)
	originalLogger := Logger
	Logger = configureLogger("error", "logfmt")
	defer func() { Logger = originalLogger }()

	e1 := newMockExporter(true, false)
	e2 := newMockExporter(true, true)
	setCurrentExporter(e1)

	var failed atomic.Int32
	var wg sync.WaitGroup

	// Concurrent simulated reloads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			ReloadLock.Lock()
			if i%2 == 0 {
				setCurrentExporter(e2)
			} else {
				setCurrentExporter(e1)
			}
			ReloadLock.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	// Concurrent health/status requests.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				req := httptest.NewRequest(http.MethodGet, "/up", nil)

				w1 := httptest.NewRecorder()
				e1.UpCheckFunc(w1, req)
				if w1.Code != http.StatusOK && w1.Code != http.StatusServiceUnavailable {
					failed.Add(1)
				}

				w2 := httptest.NewRecorder()
				e1.PrimaryCheckFunc(w2, req)
				if w2.Code != http.StatusOK && w2.Code != http.StatusNotFound && w2.Code != http.StatusServiceUnavailable {
					failed.Add(1)
				}

				w3 := httptest.NewRecorder()
				e1.ReplicaCheckFunc(w3, req)
				if w3.Code != http.StatusOK && w3.Code != http.StatusNotFound && w3.Code != http.StatusServiceUnavailable {
					failed.Add(1)
				}

				w4 := httptest.NewRecorder()
				e1.StatFunc(w4, req)
				if w4.Code != http.StatusOK && w4.Code != http.StatusServiceUnavailable {
					failed.Add(1)
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent reload/health requests timed out, possible deadlock")
	}

	if failed.Load() > 0 {
		t.Fatalf("unexpected HTTP status count: %d", failed.Load())
	}
}
