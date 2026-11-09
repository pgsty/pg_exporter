package exporter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type contextTestConnector struct {
	query func(context.Context, string) (driver.Rows, error)
}

func (c contextTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &contextTestConn{query: c.query}, nil
}
func (contextTestConnector) Driver() driver.Driver { return histogramTestDriver{} }

type boolTestRows struct{ *histogramTestRows }

func (*boolTestRows) ColumnTypeDatabaseTypeName(int) string { return "BOOL" }

type contextTestConn struct {
	histogramTestConn
	query func(context.Context, string) (driver.Rows, error)
}

func (c *contextTestConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return c.query(ctx, q)
}

func contextTestServer(t *testing.T, query func(context.Context, string) (driver.Rows, error)) *Server {
	t.Helper()
	s := NewServer("postgres://monitor@localhost/postgres", WithServerConnectTimeout(20))
	s.DB = sql.OpenDB(contextTestConnector{query: query})
	t.Cleanup(func() { _ = s.DB.Close() })
	s.beforeScrape = func(context.Context, *Server) error { s.UP = true; return nil }
	s.Planned = true
	s.ResetStats()
	return s
}

func contextTestCollector(t *testing.T, s *Server, definition string) *Collector {
	t.Helper()
	q, err := ParseQuery(definition)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(q, s)
	s.Collectors = append(s.Collectors, c)
	s.ResetStats()
	return c
}

const blockingCollector = "slow:\n  query: SELECT slow\n  ttl: 60\n  timeout: -1\n  metrics:\n    - value: {usage: GAUGE}\n"

func scalarRows(value driver.Value) driver.Rows {
	return &histogramTestRows{columns: []string{"value"}, values: [][]driver.Value{{value}}}
}

func awaitTest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not complete")
	}
}

func TestHealthProbeDoesNotWaitForScrape(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var state atomic.Int32
	s := contextTestServer(t, func(ctx context.Context, query string) (driver.Rows, error) {
		if query == "SELECT slow" {
			close(entered)
			select {
			case <-release:
				return scalarRows(int64(1)), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		switch state.Load() {
		case 2:
			return nil, errors.New("unreachable")
		case 3:
			return nil, &pq.Error{Code: pgSQLStateCannotConnectNow}
		default:
			return scalarRows(state.Load() == 1), nil
		}
	})
	contextTestCollector(t, s, blockingCollector)
	e := &Exporter{server: s, disableIntro: true}
	done := make(chan struct{})
	go func() { e.Collect(make(chan prometheus.Metric, 8)); close(done) }()
	defer func() { close(release); awaitTest(t, done) }()
	awaitTest(t, entered)
	for i, want := range []string{"primary", "replica", "down", "starting", "primary"} {
		state.Store(int32(i % 4))
		probed := make(chan struct{})
		go func() { _ = e.probeAndUpdateHealthState(); close(probed) }()
		awaitTest(t, probed)
		if got := e.Status(); got != want {
			t.Fatalf("status = %s, want %s", got, want)
		}
	}
	// Stopping the health loop must also finish while collection is blocked.
	e.startHealthLoop()
	stopped := make(chan struct{})
	go func() { e.stopHealthLoop(); close(stopped) }()
	awaitTest(t, stopped)
}

func TestOldScrapeCannotOverwriteNewHealth(t *testing.T) {
	for _, want := range []string{"primary", "replica", "down"} {
		t.Run(want, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			primary := contextTestServer(t, func(context.Context, string) (driver.Rows, error) {
				if want == "down" {
					return nil, errors.New("unreachable")
				}
				return scalarRows(want == "replica"), nil
			})
			primary.Recovery = want != "replica" // older observation differs from probe
			child := contextTestServer(t, func(context.Context, string) (driver.Rows, error) {
				close(entered)
				<-release
				return scalarRows(int64(1)), nil
			})
			contextTestCollector(t, child, blockingCollector)
			e := &Exporter{server: primary, servers: map[string]*Server{"child": child}, disableIntro: true}
			done := make(chan struct{})
			go func() { e.Collect(make(chan prometheus.Metric, 8)); close(done) }()
			awaitTest(t, entered)
			_ = e.probeAndUpdateHealthState()
			close(release)
			awaitTest(t, done)
			if e.Status() != want {
				t.Fatalf("old scrape replaced %s with %s", want, e.Status())
			}
		})
	}
}

func TestHealthPoolWaitIsBounded(t *testing.T) {
	s := contextTestServer(t, func(context.Context, string) (driver.Rows, error) { return scalarRows(false), nil })
	s.DB.SetMaxOpenConns(1)
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		up, _, _, err := s.ProbeHealth()
		if up || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("probe using exhausted pool: up=%v err=%v", up, err)
		}
		close(done)
	}()
	awaitTest(t, done)
}

func TestMetricsCancellationKeepsFailureTTL(t *testing.T) {
	entered := make(chan struct{}, 1)
	var slow atomic.Bool
	var calls atomic.Int32
	server := contextTestServer(t, func(ctx context.Context, query string) (driver.Rows, error) {
		if query == "SELECT slow" {
			calls.Add(1)
			if slow.Load() {
				entered <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		return scalarRows(int64(7)), nil
	})
	front := contextTestCollector(t, server, strings.ReplaceAll(blockingCollector, "slow", "front"))
	failed := contextTestCollector(t, server, blockingCollector)
	tail := contextTestCollector(t, server, strings.ReplaceAll(blockingCollector, "slow", "tail"))
	extra := prometheus.NewRegistry()
	extra.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "other_metric", Help: "test"}))
	h := newMetricsHandler(&Exporter{server: server, disableIntro: true}, extra)
	scrape := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "0.1")
		h.ServeHTTP(w, r)
		return w
	}
	if w := scrape(); w.Code != 200 {
		t.Fatal(w.Code)
	}
	failed.lastScrape = time.Now().Add(-2 * time.Minute)
	slow.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx)); close(done) }()
	awaitTest(t, entered)
	cancel()
	awaitTest(t, done)
	if w.Code != 503 || strings.Contains(w.Body.String(), "# TYPE") {
		t.Fatalf("canceled response: %d %s", w.Code, w.Body.String())
	}
	if failed.lastScrape.IsZero() || failed.ResultSize() != 0 || failed.Error() == nil {
		t.Fatal("cancellation lost the failure TTL")
	}
	if front.ResultSize() != 1 || tail.ResultSize() != 1 {
		t.Fatal("unrelated caches were cleared")
	}
	for range 6 {
		w = scrape()
		if w.Code != 200 || strings.Contains(w.Body.String(), "slow_value") || !strings.Contains(w.Body.String(), "front_value 7") || !strings.Contains(w.Body.String(), "tail_value 7") || !strings.Contains(w.Body.String(), "other_metric") {
			t.Fatalf("failure cache did not isolate the canceled collector: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("canceled SQL repeated within TTL: %d calls", calls.Load())
	}
	slow.Store(false)
	failed.lastScrape = time.Now().Add(-2 * time.Minute)
	if w = scrape(); w.Code != 200 || !strings.Contains(w.Body.String(), "slow_value 7") {
		t.Fatalf("retry after TTL: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
}

// A client leaving during a bounded query must not turn a healthy refresh into a
// failure cached for every scraper, nor count as a target error. The query
// finishes on its own deadline, so a fatal collector keeps pg_up and the
// remaining metrics intact for the next scraper.
func TestClientCancelLetsBoundedRefreshFinish(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var block atomic.Bool
	var calls atomic.Int32
	server := contextTestServer(t, func(ctx context.Context, query string) (driver.Rows, error) {
		if query == "SELECT fatal" {
			calls.Add(1)
			if block.Load() {
				entered <- struct{}{}
				select { // completes only after the client has left
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return scalarRows(int64(7)), nil
	})
	fatal := contextTestCollector(t, server, "fatal:\n  query: SELECT fatal\n  ttl: 60\n  timeout: 1\n  fatal: true\n  metrics:\n    - value: {usage: GAUGE}\n")
	contextTestCollector(t, server, "other:\n  query: SELECT other\n  ttl: 60\n  metrics:\n    - value: {usage: GAUGE}\n")
	e := &Exporter{server: server}
	e.setupInternalMetrics()
	h := newMetricsHandler(e, prometheus.NewRegistry())

	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, done := httptest.NewRecorder(), make(chan struct{})
	go func() { h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx)); close(done) }()
	awaitTest(t, entered)
	cancel()
	close(release)
	awaitTest(t, done)
	block.Store(false)
	if w.Code != 503 {
		t.Fatalf("canceled response: %d", w.Code)
	}
	if fatal.Error() != nil || fatal.ResultSize() != 1 || fatal.lastScrape.IsZero() {
		t.Fatalf("client cancellation poisoned a bounded refresh: err=%v results=%d", fatal.Error(), fatal.ResultSize())
	}
	if !server.UP || server.errorCount != 0 {
		t.Fatalf("client cancellation counted as a target error: up=%v errors=%v", server.UP, server.errorCount)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{"pg_up 1", "fatal_value 7", "other_value 7", "pg_exporter_scrape_error_count 0", `pg_exporter_server_scrape_error_count{datname="postgres"} 0`} {
		if w.Code != 200 || !strings.Contains(body, want) {
			t.Fatalf("next scraper: HTTP %d, missing %q in\n%s", w.Code, want, body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("completed refresh repeated within TTL: %d calls", calls.Load())
	}
}

// Once the request ends, the collector already running finishes its predicates
// and main query under its single deadline and is cached; no further collector
// or database starts. Interrupted prechecks are not target errors either.
func TestCanceledRequestFinishesStartedCollectorOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	s := contextTestServer(t, func(_ context.Context, q string) (driver.Rows, error) {
		calls = append(calls, q)
		if strings.HasPrefix(q, "SELECT pred") {
			if q == "SELECT pred1" {
				cancel()
			}
			return &boolTestRows{&histogramTestRows{columns: []string{"pass"}, values: [][]driver.Value{{true}}}}, nil
		}
		return scalarRows(int64(7)), nil
	})
	started := contextTestCollector(t, s, "started:\n  query: SELECT main\n  timeout: 1\n  predicate_queries:\n    - predicate_query: SELECT pred1\n    - predicate_query: SELECT pred2\n  metrics:\n    - value: {usage: GAUGE}\n")
	contextTestCollector(t, s, "next:\n  query: SELECT next\n  timeout: 1\n  metrics:\n    - value: {usage: GAUGE}\n")
	s.collect(ctx, make(chan prometheus.Metric, 8))
	if strings.Join(calls, ",") != "SELECT pred1,SELECT pred2,SELECT main" {
		t.Fatalf("unexpected SQL after cancellation: %v", calls)
	}
	if started.Error() != nil || started.ResultSize() != 1 {
		t.Fatalf("started collector not cached: err=%v results=%d", started.Error(), started.ResultSize())
	}
	if !s.interrupted || !s.UP || s.errorCount != 0 {
		t.Fatalf("interrupted pass counted as target error: interrupted=%v up=%v errors=%v", s.interrupted, s.UP, s.errorCount)
	}

	canceled, stop := context.WithCancel(context.Background())
	defer stop()
	prechecked := false
	s.beforeScrape = func(c context.Context, s *Server) error {
		prechecked = true
		s.UP = false
		stop()
		return c.Err()
	}
	s.collect(canceled, make(chan prometheus.Metric, 8))
	if !prechecked || !s.interrupted || !s.UP || s.errorCount != 0 {
		t.Fatalf("interrupted precheck counted as target error: up=%v errors=%v", s.UP, s.errorCount)
	}

	// A real connection failure is still a target error.
	s.beforeScrape = func(context.Context, *Server) error { return errors.New("connection refused") }
	s.collect(context.Background(), make(chan prometheus.Metric, 8))
	if s.interrupted || s.UP || s.errorCount != 1 {
		t.Fatalf("real failure not counted: interrupted=%v up=%v errors=%v", s.interrupted, s.UP, s.errorCount)
	}
}

func TestInterruptedFirstPrecheckDoesNotCountExporterError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := contextTestServer(t, func(context.Context, string) (driver.Rows, error) {
		t.Fatal("collector SQL must not run after an interrupted precheck")
		return nil, nil
	})
	s.beforeScrape = func(c context.Context, s *Server) error {
		s.UP = false
		cancel()
		return c.Err()
	}
	e := &Exporter{server: s}
	e.setupInternalMetrics()
	e.collect(ctx, make(chan prometheus.Metric, 128))
	var errors dto.Metric
	if err := e.scrapeErrorCount.Write(&errors); err != nil {
		t.Fatal(err)
	}
	if !s.interrupted || s.UP || s.errorCount != 0 || errors.GetCounter().GetValue() != 0 || readGauge(t, e.up) != 0 {
		t.Fatalf("first precheck cancellation counted as a target error: interrupted=%v up=%v server_errors=%v exporter_errors=%v", s.interrupted, s.UP, s.errorCount, errors.GetCounter().GetValue())
	}

	s.beforeScrape = func(context.Context, *Server) error { return nil }
	e.collect(context.Background(), make(chan prometheus.Metric, 128))
	if err := e.scrapeErrorCount.Write(&errors); err != nil {
		t.Fatal(err)
	}
	if s.interrupted || !s.UP || s.errorCount != 0 || errors.GetCounter().GetValue() != 0 || readGauge(t, e.up) != 1 {
		t.Fatalf("next scrape did not recover: up=%v server_errors=%v exporter_errors=%v", s.UP, s.errorCount, errors.GetCounter().GetValue())
	}
}

func TestTwoMetricsScrapersWaitAndReuseCache(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{}, 1)
	var calls atomic.Int32
	server := contextTestServer(t, func(ctx context.Context, _ string) (driver.Rows, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return scalarRows(int64(7)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	collector := contextTestCollector(t, server, blockingCollector)
	h := newMetricsHandler(&Exporter{server: server, disableIntro: true}, prometheus.NewRegistry())
	for round := range 5 {
		collector.lastScrape = time.Time{}
		first, second := httptest.NewRecorder(), httptest.NewRecorder()
		firstDone, secondDone := make(chan struct{}), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		go func() {
			h.ServeHTTP(first, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
			close(firstDone)
		}()
		awaitTest(t, entered)
		go func() {
			h.ServeHTTP(second, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
			close(secondDone)
		}()
		select {
		case <-secondDone:
			cancel()
			awaitTest(t, firstDone)
			t.Fatalf("second scraper rejected: %d", second.Code)
		case <-time.After(20 * time.Millisecond):
		}
		release <- struct{}{}
		awaitTest(t, firstDone)
		awaitTest(t, secondDone)
		cancel()
		for _, w := range []*httptest.ResponseRecorder{first, second} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "slow_value 7") {
				t.Fatalf("dual scrape: %d %s", w.Code, w.Body.String())
			}
		}
		if calls.Load() != int32(round+1) {
			t.Fatalf("overlapping scrapers duplicated SQL: %d", calls.Load())
		}
	}
}

func TestMetricsWaitingCancellationAndAdmissionLimit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := contextTestServer(t, func(ctx context.Context, _ string) (driver.Rows, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return scalarRows(int64(7)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	contextTestCollector(t, server, blockingCollector)
	h := newMetricsHandler(&Exporter{server: server, disableIntro: true}, prometheus.NewRegistry())
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first, firstDone := httptest.NewRecorder(), make(chan struct{})
	go func() {
		h.ServeHTTP(first, httptest.NewRequest("GET", "/metrics", nil).WithContext(firstCtx))
		close(firstDone)
	}()
	awaitTest(t, entered)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	waiter, waitDone := httptest.NewRecorder(), make(chan struct{})
	go func() {
		h.ServeHTTP(waiter, httptest.NewRequest("GET", "/metrics", nil).WithContext(waitCtx))
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatalf("waiter rejected: %d", waiter.Code)
	case <-time.After(20 * time.Millisecond):
	}
	third := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "1")
	start := time.Now()
	h.ServeHTTP(third, r)
	if third.Code != 503 || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("full admission did not reject promptly: %d", third.Code)
	}
	cancelWait()
	awaitTest(t, waitDone)
	if waiter.Code != 503 || calls.Load() != 1 {
		t.Fatalf("canceled waiter executed SQL: code=%d calls=%d", waiter.Code, calls.Load())
	}
	// Cancellation releases the waiting slot while leaving the active scrape alone.
	replacement, replacementDone := httptest.NewRecorder(), make(chan struct{})
	go func() { h.ServeHTTP(replacement, httptest.NewRequest("GET", "/metrics", nil)); close(replacementDone) }()
	select {
	case <-replacementDone:
		t.Fatalf("waiting slot leaked: %d", replacement.Code)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	awaitTest(t, firstDone)
	awaitTest(t, replacementDone)
	if first.Code != 200 || replacement.Code != 200 || calls.Load() != 1 {
		t.Fatalf("scrape after canceled waiter: %d %d calls=%d", first.Code, replacement.Code, calls.Load())
	}
}

type blockedMetricsWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *blockedMetricsWriter) Write(b []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestSlowMetricsResponseReleasesCollectionLock(t *testing.T) {
	var calls atomic.Int32
	server := contextTestServer(t, func(context.Context, string) (driver.Rows, error) { calls.Add(1); return scalarRows(int64(7)), nil })
	contextTestCollector(t, server, blockingCollector)
	exporter := &Exporter{server: server, disableIntro: true}
	h := newMetricsHandler(exporter, prometheus.NewRegistry())
	w := &blockedMetricsWriter{httptest.NewRecorder(), make(chan struct{}, 1), make(chan struct{})}
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil)); close(done) }()
	defer func() { close(w.release); awaitTest(t, done) }()
	awaitTest(t, w.entered)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := exporter.lock.LockContext(ctx); err != nil {
		t.Fatalf("slow response blocks reload/close: %v", err)
	}
	exporter.lock.Unlock()
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", "/metrics", nil))
	if second.Code != 200 || calls.Load() != 1 {
		t.Fatalf("slow reader blocked cached scrape: %d SQL=%d", second.Code, calls.Load())
	}
}

func TestWaitingDeadlinePreservesValidCache(t *testing.T) {
	var calls atomic.Int32
	server := contextTestServer(t, func(context.Context, string) (driver.Rows, error) {
		calls.Add(1)
		return scalarRows(int64(7)), nil
	})
	collector := contextTestCollector(t, server, blockingCollector)
	exporter := &Exporter{server: server, disableIntro: true}
	h := newMetricsHandler(exporter, prometheus.NewRegistry())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	lastScrape := collector.lastScrape
	exporter.lock.Lock() // configuration replacement can also hold this lock
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "0.02")
	w, done := httptest.NewRecorder(), make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		exporter.lock.Unlock()
		awaitTest(t, done)
		t.Fatal("waiting did not consume the request budget")
	}
	exporter.lock.Unlock()
	if w.Code != 503 || calls.Load() != 1 || collector.ResultSize() != 1 || !collector.lastScrape.Equal(lastScrape) {
		t.Fatalf("canceled waiter changed cache: code=%d SQL=%d results=%d", w.Code, calls.Load(), collector.ResultSize())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("waiting slot or cache lost after timeout: code=%d SQL=%d", w.Code, calls.Load())
	}
}

func TestPgBouncerHealthHasConnectionDuringScrape(t *testing.T) {
	server := NewServer("postgres://monitor@localhost/pgbouncer", WithServerConnectTimeout(50))
	if err := server.initDB(); err != nil {
		t.Fatal(err)
	}
	capacity := server.DB.Stats().MaxOpenConnections
	_ = server.DB.Close()
	if capacity != 2 {
		t.Fatalf("PgBouncer connection capacity: %d", capacity)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var probes atomic.Int32
	var unreachable atomic.Bool
	server.DB = sql.OpenDB(contextTestConnector{query: func(ctx context.Context, query string) (driver.Rows, error) {
		if query == "SELECT slow" {
			close(entered)
			select {
			case <-release:
				return scalarRows(int64(1)), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		probes.Add(1)
		if unreachable.Load() {
			return nil, errors.New("unreachable")
		}
		return scalarRows("PgBouncer 1.26.1"), nil
	}})
	server.DB.SetMaxOpenConns(capacity)
	t.Cleanup(func() { _ = server.DB.Close() })
	server.beforeScrape = func(context.Context, *Server) error { server.UP = true; return nil }
	server.Planned = true
	contextTestCollector(t, server, blockingCollector)
	exporter := &Exporter{server: server, disableIntro: true}
	done := make(chan struct{})
	go func() { exporter.Collect(make(chan prometheus.Metric, 8)); close(done) }()
	defer func() { close(release); awaitTest(t, done) }()
	awaitTest(t, entered)
	for _, down := range []bool{false, true, false} {
		unreachable.Store(down)
		err := exporter.probeAndUpdateHealthState()
		if down {
			if err == nil || exporter.Status() != "down" {
				t.Fatalf("target failure hidden: %v %s", err, exporter.Status())
			}
		} else if err != nil || exporter.Status() != "primary" {
			t.Fatalf("collection caused false down: %v %s", err, exporter.Status())
		}
	}
	if probes.Load() != 3 {
		t.Fatalf("health probes did not reach target: %d", probes.Load())
	}
}

func TestScrapeBudgetCancelsPredicatesAndPrechecks(t *testing.T) {
	for _, stage := range []string{"predicate", "postgres", "pgbouncer"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			s := contextTestServer(t, func(ctx context.Context, query string) (driver.Rows, error) {
				calls.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			if stage == "predicate" {
				contextTestCollector(t, s, "slow:\n  query: SELECT main\n  timeout: -1\n  predicate_queries:\n    - predicate_query: SELECT predicate\n  metrics:\n    - value: {usage: GAUGE}\n")
			} else {
				s.ConnectTimeout = 1000
				s.beforeScrape = postgresPrecheck
				if stage == "pgbouncer" {
					s.beforeScrape = pgbouncerPrecheck
				}
			}
			e := &Exporter{server: s, disableIntro: true}
			r := httptest.NewRequest("GET", "/metrics", nil)
			r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "0.02")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { newMetricsHandler(e, prometheus.NewRegistry()).ServeHTTP(w, r); close(done) }()
			awaitTest(t, done)
			if w.Code != 503 || calls.Load() != 1 {
				t.Fatalf("budget not propagated: HTTP %d, SQL calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestScrapeBudgetHeader(t *testing.T) {
	for _, value := range []string{"", "bad", "NaN", "+Inf", "-Inf", "-1", "0", "100000000000"} {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", value)
		if got := scrapeBudget(r); got != httpWriteTimeout {
			t.Errorf("header %q: budget=%s", value, got)
		}
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "1.5")
	if got := scrapeBudget(r); got != 1500*time.Millisecond {
		t.Fatalf("fractional timeout=%s", got)
	}
	r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "28")
	if got := scrapeBudget(r); got != 28*time.Second {
		t.Fatalf("unexpected cap below the HTTP write timeout: %s", got)
	}
}

type cancelDuringRows struct {
	ctx   context.Context
	first bool
}

func (*cancelDuringRows) Columns() []string { return []string{"value"} }
func (*cancelDuringRows) Close() error      { return nil }
func (r *cancelDuringRows) Next(values []driver.Value) error {
	if !r.first {
		r.first = true
		values[0] = int64(1)
		return nil
	}
	<-r.ctx.Done()
	return io.EOF
}

func TestCanceledRowsCacheFailureWithoutPublishingPartialResults(t *testing.T) {
	s := contextTestServer(t, func(ctx context.Context, _ string) (driver.Rows, error) {
		return &cancelDuringRows{ctx: ctx}, nil
	})
	c := contextTestCollector(t, s, blockingCollector)
	s.scrapeBegin = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ch := make(chan prometheus.Metric, 8)
	c.collect(ctx, ch)
	if c.Error() == nil || c.ResultSize() != 0 || len(ch) != 0 || c.lastScrape.IsZero() {
		t.Fatalf("partial canceled snapshot: err=%v results=%d emitted=%d cache=%s", c.Error(), c.ResultSize(), len(ch), c.lastScrape)
	}
}

func TestScrapeBudgetSpansAllCollectors(t *testing.T) {
	var firstDeadline time.Time
	var calls atomic.Int32
	s := contextTestServer(t, func(ctx context.Context, _ string) (driver.Rows, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("missing collection deadline")
		}
		if calls.Add(1) == 1 {
			firstDeadline = deadline
			return scalarRows(int64(1)), nil
		}
		if !deadline.Equal(firstDeadline) {
			t.Error("second query reset the scrape budget")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	for _, name := range []string{"first", "second", "third"} {
		contextTestCollector(t, s, strings.Replace(blockingCollector, "slow:", name+":", 1))
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "0.05")
	w := httptest.NewRecorder()
	newMetricsHandler(&Exporter{server: s, disableIntro: true}, prometheus.NewRegistry()).ServeHTTP(w, r)
	if w.Code != 503 || calls.Load() != 2 {
		t.Fatalf("scrape budget: HTTP=%d queries=%d", w.Code, calls.Load())
	}
}

func TestProbeDuringDBInitialization(t *testing.T) {
	s := NewServer("postgres://monitor@127.0.0.1:1/postgres?sslmode=disable", WithServerConnectTimeout(5))
	done := make(chan struct{})
	go func() {
		for range 20 {
			_, _, _, _ = s.ProbeHealth()
		}
		close(done)
	}()
	for range 20 {
		if err := s.initDB(); err != nil {
			t.Fatal(err)
		}
	}
	awaitTest(t, done)
	_ = s.DB.Close()
}
