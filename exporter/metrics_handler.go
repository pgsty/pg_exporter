package exporter

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

const httpWriteTimeout = 30 * time.Second

// collectionLock is a zero-value-ready mutex with cancelable acquisition for
// HTTP callers. Ordinary collection, reload, and close use the same lock.
type collectionLock struct {
	once  sync.Once
	token chan struct{}
}

func (l *collectionLock) Lock() {
	_ = l.LockContext(context.Background())
}

func (l *collectionLock) LockContext(ctx context.Context) error {
	l.once.Do(func() { l.token = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			l.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *collectionLock) Unlock() { <-l.token }

type requestCollector struct {
	*Exporter
	ctx context.Context
}

func (c requestCollector) Collect(ch chan<- prometheus.Metric) {
	c.Exporter.collect(c.ctx, ch)
}

func scrapeBudget(r *http.Request) time.Duration {
	seconds, err := strconv.ParseFloat(r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"), 64)
	if err == nil && !math.IsNaN(seconds) && seconds > 0 && seconds < httpWriteTimeout.Seconds() {
		return time.Duration(seconds * float64(time.Second))
	}
	return httpWriteTimeout
}

// newMetricsHandler admits two requests, including waiters and slow responses.
// SQL remains serial, while a second scraper can wait and reuse valid caches.
func newMetricsHandler(e *Exporter, extra prometheus.Gatherer) http.Handler {
	inFlight := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), scrapeBudget(r))
		defer cancel()
		select {
		case inFlight <- struct{}{}:
			defer func() { <-inFlight }()
		default:
			http.Error(w, "exporter busy; retry later", http.StatusServiceUnavailable)
			return
		}
		families, err := gatherMetrics(ctx, e, extra)
		if ctx.Err() != nil {
			http.Error(w, "scrape canceled or timed out", http.StatusServiceUnavailable)
			return
		}
		promhttp.HandlerFor(prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
			return families, err
		}), promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}

func gatherMetrics(ctx context.Context, e *Exporter, extra prometheus.Gatherer) ([]*dto.MetricFamily, error) {
	if err := e.lock.LockContext(ctx); err != nil {
		return nil, err
	}
	defer e.lock.Unlock() // release before encoding or network writes
	registry := prometheus.NewRegistry()
	registry.MustRegister(requestCollector{Exporter: e, ctx: ctx})
	return (prometheus.Gatherers{extra, registry}).Gather()
}
