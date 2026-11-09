package exporter

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

/* ================ Exporter ================ */

const (
	healthStatusUnknown int32 = iota
	healthStatusDown
	healthStatusStarting
	healthStatusPrimary
	healthStatusReplica
)

// Exporter implement prometheus.Collector interface
// exporter contains one or more (auto-discover-database) servers that can scrape metrics with Query
type Exporter struct {
	// config params provided from ExporterOpt
	dsn             string            // primary dsn
	configPath      string            // config file path /directory
	configReader    io.Reader         // reader to a config file, one of configPath or configReader must be set
	disableCache    bool              // always execute query when been scraped
	disableIntro    bool              // disable internal/exporter self metrics (only expose query metrics)
	autoDiscovery   bool              // discovery other database on primary server
	pgbouncerMode   bool              // is primary server a pgbouncer ?
	failFast        bool              // fail fast instead fof waiting during start-up ?
	excludeDatabase map[string]bool   // excluded database for auto discovery
	includeDatabase map[string]bool   // include database for auto discovery
	constLabels     prometheus.Labels // prometheus const k=v labels
	tags            []string          // tags passed to this exporter for scheduling purpose
	namespace       string            // metrics prefix ('pg' or 'pgbouncer' by default)
	connectTimeout  int               // timeout in ms when perform server pre-check

	// internal status
	lock    collectionLock     // serializes collection, reload, and close
	server  *Server            // primary server
	sLock   sync.RWMutex       // server map lock
	servers map[string]*Server // auto discovered peripheral servers
	queries map[string]*Query  // metrics query definition

	// internal stats
	scrapeBegin time.Time // server level scrape begin
	scrapeDone  time.Time // server last scrape done

	// internal metrics: global, exporter, server, query
	up               prometheus.Gauge   // cluster level: primary target server is alive
	version          prometheus.Gauge   // cluster level: postgres main server version num
	recovery         prometheus.Gauge   // cluster level: postgres is in recovery ?
	buildInfo        prometheus.Gauge   // exporter level: build information
	exporterUp       prometheus.Gauge   // exporter level: always set ot 1
	exporterUptime   prometheus.Gauge   // exporter level: primary target server uptime (exporter itself)
	lastScrapeTime   prometheus.Gauge   // exporter level: last scrape timestamp
	scrapeDuration   prometheus.Gauge   // exporter level: seconds spend on scrape
	scrapeTotalCount prometheus.Counter // exporter level: total scrape count of this server
	scrapeErrorCount prometheus.Counter // exporter level: error scrape count

	// Dynamic series (auto-discovered DBs, config reload) are emitted as const
	// metrics on each scrape to avoid GaugeVec Reset() overhead and stale series.
	serverScrapeDurationDesc     *prometheus.Desc // {datname} database level: last scrape duration
	serverScrapeTotalSecondsDesc *prometheus.Desc // {datname} database level: cumulative scrape seconds
	serverScrapeTotalCountDesc   *prometheus.Desc // {datname} database level: total scrape count
	serverScrapeErrorCountDesc   *prometheus.Desc // {datname} database level: cumulative fatal scrape error count

	queryCacheTTLDesc                 *prometheus.Desc // {datname,query} query cache ttl
	queryScrapeTotalCountDesc         *prometheus.Desc // {datname,query} query level: total executions
	queryScrapeErrorCountDesc         *prometheus.Desc // {datname,query} query level: error count
	queryScrapePredicateSkipCountDesc *prometheus.Desc // {datname,query} query level: predicate skip count
	queryScrapeDurationDesc           *prometheus.Desc // {datname,query} query level: execution duration (seconds)
	queryScrapeMetricCountDesc        *prometheus.Desc // {datname,query} query level: returned metric count
	queryScrapeHitCountDesc           *prometheus.Desc // {datname,query} query level: cache hit count

	// lock-free health snapshot for high-frequency probes
	healthStatus    atomic.Int32
	healthProbeLock sync.Mutex // serialize explicit and background probes

	healthLoopLock sync.Mutex
	healthLoopStop chan struct{}
	healthLoopDone chan struct{}
}

// Up will delegate aliveness check to primary server
func (e *Exporter) Up() bool {
	status := e.healthStatus.Load()
	return status == healthStatusPrimary || status == healthStatusReplica
}

// Recovery will delegate primary/replica check to primary server
func (e *Exporter) Recovery() bool {
	return e.healthStatus.Load() == healthStatusReplica
}

// Status will report available status: primary|replica|starting|down|unknown
func (e *Exporter) Status() string {
	return healthStatusText(e.healthStatus.Load())
}

func healthStatusText(status int32) string {
	switch status {
	case healthStatusPrimary:
		return `primary`
	case healthStatusReplica:
		return `replica`
	case healthStatusStarting:
		return `starting`
	case healthStatusDown:
		return `down`
	default:
		return `unknown`
	}
}

func (e *Exporter) updateHealthState(up, recovery bool) {
	e.updateHealthStateWithStartup(up, recovery, false)
}

func (e *Exporter) updateHealthStateWithStartup(up, recovery, starting bool) {
	status := healthStatusDown
	if starting {
		status = healthStatusStarting
	} else if up {
		status = healthStatusPrimary
		if recovery {
			status = healthStatusReplica
		}
	}
	e.healthStatus.Store(status)
}

func (e *Exporter) updateHealthStateFromServer() {
	if e.server == nil {
		e.healthStatus.Store(healthStatusUnknown)
		return
	}
	e.server.lock.RLock()
	up := e.server.UP
	recovery := e.server.Recovery
	e.server.lock.RUnlock()
	e.updateHealthState(up, recovery)
}

func (e *Exporter) probeAndUpdateHealthState() error {
	e.healthProbeLock.Lock()
	defer e.healthProbeLock.Unlock()
	if e.server == nil {
		e.healthStatus.Store(healthStatusUnknown)
		return errors.New("primary server is nil")
	}
	up, recovery, starting, err := e.server.ProbeHealth()
	e.updateHealthStateWithStartup(up, recovery, starting)
	return err
}

func (e *Exporter) startHealthLoop() {
	e.healthLoopLock.Lock()
	if e.healthLoopStop != nil {
		e.healthLoopLock.Unlock()
		return
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	e.healthLoopStop = stopCh
	e.healthLoopDone = doneCh
	e.healthLoopLock.Unlock()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// Health probing is intentionally decoupled from /metrics scraping so that:
		// - HTTP health handlers never block on network calls
		// - the exporter can recover health status even when scrapes are failing
		//
		// Keep Server.ProbeHealth cheap and log-noise-free: it runs once per second.
		_ = e.probeAndUpdateHealthState()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				_ = e.probeAndUpdateHealthState()
			}
		}
	}()
}

func (e *Exporter) stopHealthLoop() {
	e.healthLoopLock.Lock()
	stopCh := e.healthLoopStop
	doneCh := e.healthLoopDone
	e.healthLoopStop = nil
	e.healthLoopDone = nil
	e.healthLoopLock.Unlock()

	if stopCh == nil {
		return
	}
	close(stopCh)
	if doneCh != nil {
		<-doneCh
	}
}

// Describe implement prometheus.Collector
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// Intentionally leave this exporter "unchecked".
	//
	// Query metrics are dynamic:
	// - config reload can add/remove collectors and metrics
	// - auto-discovery can add/remove databases
	//
	// If we emitted any descriptors here, the Prometheus registry would enforce
	// that Collect() only returns described metrics, which does not hold for a
	// dynamic exporter. Exporter-toolkit and client_golang both support this
	// pattern (Describe emits nothing).
}

// Collect implement prometheus.Collector
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.collect(context.Background(), ch)
}

// collect requires e.lock. HTTP callers supply their request context.
func (e *Exporter) collect(ctx context.Context, ch chan<- prometheus.Metric) {
	if ctx.Err() != nil {
		return
	}
	if !e.disableIntro {
		e.scrapeTotalCount.Add(1)
	}

	e.scrapeBegin = time.Now()
	// scrape primary server
	s := e.server
	s.collect(ctx, ch)

	// scrape extra servers if exists
	for _, srv := range e.IterateServer() {
		if ctx.Err() != nil {
			break
		}
		srv.collect(ctx, ch)
	}
	e.scrapeDone = time.Now()

	if !e.disableIntro {
		e.lastScrapeTime.Set(float64(e.scrapeDone.Unix()))
		e.scrapeDuration.Set(e.scrapeDone.Sub(e.scrapeBegin).Seconds())
	}
	s.lock.RLock()
	version := s.Version
	up := s.UP
	recovery := s.Recovery
	interrupted := s.interrupted
	s.lock.RUnlock()

	// Only health probes publish HTTP health after construction. A scrape may
	// contain an older main-target observation while child databases are collected.
	if !e.disableIntro {
		e.version.Set(float64(version))
		if up {
			e.up.Set(1)
			if recovery {
				e.recovery.Set(1)
			} else {
				e.recovery.Set(0)
			}
		} else {
			e.up.Set(0)
			if !interrupted {
				e.scrapeErrorCount.Add(1)
			}
		}
		e.exporterUptime.Set(e.server.Uptime())
		e.collectServerMetrics(ch)
		e.collectInternalMetrics(ch)
	}
}

func (e *Exporter) collectServerMetrics(ch chan<- prometheus.Metric) {
	servers := e.IterateServer()
	if e.server != nil {
		servers = append(servers, e.server) // append primary server to extra server list
	}
	for _, s := range servers {
		if s == nil {
			continue
		}
		s.lock.RLock()
		datname := s.Database
		scrapeDur := s.scrapeDone.Sub(s.scrapeBegin).Seconds()
		totalSeconds := s.totalTime
		totalCount := s.totalCount
		errorCount := s.errorCount

		// Snapshot query maps (they are replaced as a whole on ResetStats).
		queryCacheTTL := s.queryCacheTTL
		queryScrapeTotalCount := s.queryScrapeTotalCount
		queryScrapeHitCount := s.queryScrapeHitCount
		queryScrapeErrorCount := s.queryScrapeErrorCount
		queryScrapePredicateSkipCount := s.queryScrapePredicateSkipCount
		queryScrapeMetricCount := s.queryScrapeMetricCount
		queryScrapeDuration := s.queryScrapeDuration
		s.lock.RUnlock()

		ch <- prometheus.MustNewConstMetric(e.serverScrapeDurationDesc, prometheus.GaugeValue, scrapeDur, datname)
		ch <- prometheus.MustNewConstMetric(e.serverScrapeTotalSecondsDesc, prometheus.GaugeValue, totalSeconds, datname)
		ch <- prometheus.MustNewConstMetric(e.serverScrapeTotalCountDesc, prometheus.GaugeValue, totalCount, datname)
		ch <- prometheus.MustNewConstMetric(e.serverScrapeErrorCountDesc, prometheus.GaugeValue, errorCount, datname)

		for queryName, v := range queryCacheTTL {
			ch <- prometheus.MustNewConstMetric(e.queryCacheTTLDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapeTotalCount {
			ch <- prometheus.MustNewConstMetric(e.queryScrapeTotalCountDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapeHitCount {
			ch <- prometheus.MustNewConstMetric(e.queryScrapeHitCountDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapeErrorCount {
			ch <- prometheus.MustNewConstMetric(e.queryScrapeErrorCountDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapePredicateSkipCount {
			ch <- prometheus.MustNewConstMetric(e.queryScrapePredicateSkipCountDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapeMetricCount {
			ch <- prometheus.MustNewConstMetric(e.queryScrapeMetricCountDesc, prometheus.GaugeValue, v, datname, queryName)
		}
		for queryName, v := range queryScrapeDuration {
			ch <- prometheus.MustNewConstMetric(e.queryScrapeDurationDesc, prometheus.GaugeValue, v, datname, queryName)
		}
	}
}

// Explain is a thin wrapper of server.Explain (plain text).
func (e *Exporter) Explain() string {
	return e.server.Explain()
}

// Stat is just yet another wrapper of server.Stat
func (e *Exporter) Stat() string {
	logDebugf("stats invoked")
	return e.server.Stat()
}

// Check will perform an immediate server health check
func (e *Exporter) Check() {
	if err := e.probeAndUpdateHealthState(); err != nil {
		logErrorf("exporter check failure: %s", err.Error())
	} else {
		logDebugf("exporter check ok")
	}
}

// Close will close all underlying servers
func (e *Exporter) Close() {
	e.stopHealthLoop()
	e.lock.Lock()
	defer e.lock.Unlock()

	if e.server != nil {
		if e.server.DB != nil {
			err := e.server.Close()
			if err != nil {
				logErrorf("fail closing server %s: %s", e.server.Name(), err.Error())
			}
		}
	}
	// close peripheral servers (we may skip acquire lock here)
	for _, srv := range e.IterateServer() {
		if srv != nil {
			if srv.DB != nil {
				err := srv.Close()
				if err != nil {
					logErrorf("fail closing server %s: %s", srv.Name(), err.Error())
				}
			}
		}
	}
	logInfof("pg exporter closed")
}

// setupInternalMetrics will init internal metrics
func (e *Exporter) setupInternalMetrics() {
	if e.namespace == "" {
		if e.pgbouncerMode {
			e.namespace = "pgbouncer"
		} else {
			e.namespace = "pg"
		}
	}

	// major fact
	e.up = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Name: "up", Help: "last scrape was able to connect to the server: 1 for yes, 0 for no",
	})
	e.version = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Name: "version", Help: "server version number",
	})
	e.recovery = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Name: "in_recovery", Help: "server is in recovery mode? 1 for yes 0 for no",
	})

	// build info
	buildInfoLabels := prometheus.Labels{
		"version":   Version,
		"revision":  Revision,
		"branch":    Branch,
		"builddate": BuildDate,
		"goversion": GoVersion,
		"goos":      GOOS,
		"goarch":    GOARCH,
	}
	// Merge with user-provided constant labels
	for k, v := range e.constLabels {
		buildInfoLabels[k] = v
	}
	e.buildInfo = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   e.namespace,
		Name:        "exporter_build_info",
		Help:        "A metric with a constant '1' value labeled with version, revision, branch, goversion, builddate, goos, and goarch from which pg_exporter was built.",
		ConstLabels: buildInfoLabels,
	})
	// Set the build info value
	e.buildInfo.Set(1)

	// exporter level metrics
	e.exporterUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "up", Help: "always be 1 if your could retrieve metrics",
	})
	e.exporterUptime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "uptime", Help: "seconds since exporter primary server inited",
	})
	e.scrapeTotalCount = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "scrape_total_count", Help: "times exporter was scraped for metrics",
	})
	e.scrapeErrorCount = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "scrape_error_count", Help: "times exporter was scraped for metrics and failed",
	})
	e.scrapeDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "scrape_duration", Help: "seconds exporter spending on scraping",
	})
	e.lastScrapeTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: e.namespace, ConstLabels: e.constLabels,
		Subsystem: "exporter", Name: "last_scrape_time", Help: "last scrape timestamp",
	})

	// Dynamic per-server/per-query series.
	// These are described via *prometheus.Desc and emitted as const metrics on each scrape.
	e.serverScrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_server", "scrape_duration"),
		"seconds exporter server spending on scraping last scrape",
		[]string{"datname"}, e.constLabels,
	)
	e.serverScrapeTotalSecondsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_server", "scrape_total_seconds"),
		"cumulative total seconds exporter server spending on scraping",
		[]string{"datname"}, e.constLabels,
	)
	e.serverScrapeTotalCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_server", "scrape_total_count"),
		"times exporter server was scraped for metrics",
		[]string{"datname"}, e.constLabels,
	)
	e.serverScrapeErrorCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_server", "scrape_error_count"),
		"cumulative times exporter server scrape failed (fatal scrape failures only)",
		[]string{"datname"}, e.constLabels,
	)

	e.queryCacheTTLDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "cache_ttl"),
		"times to live of query cache",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapeTotalCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_total_count"),
		"times exporter server was scraped for metrics",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapeErrorCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_error_count"),
		"times the query failed",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapePredicateSkipCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_predicate_skip_count"),
		"times the query was skipped due to a predicate returning false",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_duration"),
		"seconds query spending on scraping",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapeMetricCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_metric_count"),
		"numbers of metrics been scraped from this query",
		[]string{"datname", "query"}, e.constLabels,
	)
	e.queryScrapeHitCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(e.namespace, "exporter_query", "scrape_hit_count"),
		"numbers been scraped from this query",
		[]string{"datname", "query"}, e.constLabels,
	)

	e.exporterUp.Set(1) // always be true
	e.healthStatus.Store(healthStatusUnknown)
}

func (e *Exporter) collectInternalMetrics(ch chan<- prometheus.Metric) {
	ch <- e.up
	ch <- e.version
	ch <- e.recovery

	ch <- e.buildInfo
	ch <- e.exporterUp
	ch <- e.exporterUptime
	ch <- e.lastScrapeTime
	ch <- e.scrapeTotalCount
	ch <- e.scrapeErrorCount
	ch <- e.scrapeDuration
}

/* ================ Exporter Creation ================ */

// NewExporter construct a PG Exporter instance for given dsn
func NewExporter(dsn string, opts ...ExporterOpt) (e *Exporter, err error) {
	return newExporterWithServerFactory(dsn, NewServer, opts...)
}

func newExporterWithServerFactory(dsn string, serverFactory func(string, ...ServerOpt) *Server, opts ...ExporterOpt) (e *Exporter, err error) {
	e = &Exporter{dsn: dsn}
	e.servers = make(map[string]*Server)
	for _, opt := range opts {
		opt(e)
	}
	if len(e.configPath) > 0 && e.configReader != nil {
		return nil, errors.New("exporter configPath and configReader options are mutually exclusive")
	}
	if len(e.configPath) > 0 {
		if e.queries, err = LoadConfig(e.configPath); err != nil {
			return nil, fmt.Errorf("fail loading config file %s: %w", e.configPath, err)
		}
	}
	if e.configReader != nil {
		b, rerr := io.ReadAll(e.configReader)
		if rerr != nil {
			return nil, fmt.Errorf("fail reading config file: %w", rerr)
		}
		if e.queries, err = ParseConfig(b); err != nil {
			return nil, fmt.Errorf("fail parsing config file: %w", err)
		}
		if err := FinalizeQueries(e.queries, "<reader>"); err != nil {
			return nil, fmt.Errorf("fail finalizing config: %w", err)
		}
	}

	if err := validateConstLabelConflicts(e.constLabels, e.queries, e.disableIntro); err != nil {
		return nil, fmt.Errorf("invalid constant labels: %w", err)
	}

	logDebugf("exporter init with %d queries", len(e.queries))

	// note here the server is still not connected. it will trigger connecting when being scraped
	e.server = serverFactory(
		dsn,
		WithQueries(e.queries),
		WithConstLabel(e.constLabels),
		WithCachePolicy(e.disableCache),
		WithServerTags(e.tags),
		WithServerConnectTimeout(e.connectTimeout),
	)

	// register db change callback
	if e.autoDiscovery {
		logInfof("auto discovery is enabled, excludeDatabase=%v, includeDatabase=%v", e.excludeDatabase, e.includeDatabase)
		e.server.onDatabaseChange = e.OnDatabaseChange
	}

	logDebugf("check primary server connectivity")
	// Best-effort check: we don't block the exporter startup if the target is down.
	// The actual scrape path will reconnect and re-plan when the target comes back.
	if err = e.server.Check(); err != nil {
		if e.failFast {
			if e.server.DB != nil {
				if closeErr := e.server.Close(); closeErr != nil {
					logErrorf("fail closing primary server after connectivity check failure: %s", closeErr.Error())
				}
			}
			return nil, fmt.Errorf("fail connecting to primary server: %w", err)
		}
		logErrorf("fail connecting to primary server: %s (startup will continue)", err.Error())
		// NewExporter has named return values; make sure we don't propagate the
		// precheck error when failFast is disabled.
		err = nil
	}
	e.pgbouncerMode = e.server.PgbouncerMode
	e.setupInternalMetrics()
	e.updateHealthStateFromServer()
	// Always start the health loop so probes can recover once the target becomes reachable.
	e.startHealthLoop()

	return
}

// OnDatabaseChange will spawn new Server when new database is created
// and destroy Server if corresponding database is dropped
func (e *Exporter) OnDatabaseChange(change map[string]bool) {
	for dbname, add := range change {
		verb := "del"
		if add {
			verb = "add"
		}

		if dbname == e.server.Database {
			continue // skip primary database change
		}
		if _, found := e.excludeDatabase[dbname]; found {
			logInfof("skip database change: %v %v according to in excluded database list", verb, dbname)
			continue // skip exclude databases changes
		}
		if len(e.includeDatabase) > 0 {
			if _, found := e.includeDatabase[dbname]; !found {
				logInfof("skip database change: %v %v according to not in include database list", verb, dbname)
				continue // skip non-include databases changes
			}
		}
		if add {
			// spawn new server
			e.CreateServer(dbname)
		} else {
			// close old server
			e.RemoveServer(dbname)
		}
	}
}

// CreateServer will spawn new database server from a database name combined with existing dsn string
// This happens when a database is newly created
func (e *Exporter) CreateServer(dbname string) {
	newDSN := ReplaceDatname(e.dsn, dbname)
	logInfof("spawn new server for database %s : %s", dbname, ShadowPGURL(newDSN))
	newServer := NewServer(
		newDSN,
		WithQueries(e.queries),
		WithConstLabel(e.constLabels),
		WithCachePolicy(e.disableCache),
		WithServerTags(e.tags),
		WithServerConnectTimeout(e.connectTimeout),
	)
	newServer.Forked = true // important!

	e.sLock.Lock()
	e.servers[dbname] = newServer
	logInfof("database %s is installed due to auto-discovery", dbname)
	defer e.sLock.Unlock()
}

// RemoveServer will destroy Server from Exporter according to database name
// This happens when a database is dropped
func (e *Exporter) RemoveServer(dbname string) {
	e.sLock.Lock()
	srv, ok := e.servers[dbname]
	if ok {
		delete(e.servers, dbname)
	}
	e.sLock.Unlock()

	if ok && srv != nil {
		if srv.DB != nil {
			// Close asynchronously to avoid blocking the scrape path.
			go func(dbname string, srv *Server) {
				if err := srv.Close(); err != nil {
					logErrorf("fail closing removed database server %s: %s", dbname, err.Error())
				}
			}(dbname, srv)
		}
	}
	logWarnf("database %s is removed due to auto-discovery", dbname)
}

// IterateServer will get snapshot of extra servers
func (e *Exporter) IterateServer() (res []*Server) {
	e.sLock.RLock()
	defer e.sLock.RUnlock()
	if len(e.servers) == 0 {
		return nil
	}
	res = make([]*Server, 0, len(e.servers))
	for _, srv := range e.servers {
		res = append(res, srv)
	}
	return
}

// ExporterOpt configures Exporter
type ExporterOpt func(*Exporter)

// WithConfig add config path to Exporter
func WithConfig(configPath string) ExporterOpt {
	return func(e *Exporter) {
		e.configPath = configPath
	}
}

// WithConfigReader uses a the provided reader to load a configuration for the Exporter
func WithConfigReader(reader io.Reader) ExporterOpt {
	return func(e *Exporter) {
		e.configReader = reader
	}
}

// WithConstLabels add const label to exporter. 0 length label returns nil
func WithConstLabels(s string) ExporterOpt {
	return func(e *Exporter) {
		e.constLabels = parseConstLabels(s)
	}
}

// WithCacheDisabled set cache param to exporter
func WithCacheDisabled(disableCache bool) ExporterOpt {
	return func(e *Exporter) {
		e.disableCache = disableCache
	}
}

// WithIntroDisabled will pass introspection option to server
func WithIntroDisabled(disableIntro bool) ExporterOpt {
	return func(s *Exporter) {
		s.disableIntro = disableIntro
	}
}

// WithFailFast marks exporter fail instead of waiting during start-up
func WithFailFast(failFast bool) ExporterOpt {
	return func(e *Exporter) {
		e.failFast = failFast
	}
}

// WithNamespace will specify metric namespace, by default is pg or pgbouncer
func WithNamespace(namespace string) ExporterOpt {
	return func(e *Exporter) {
		e.namespace = namespace
	}
}

// WithTags will register given tags to Exporter and all belonged servers
func WithTags(tags string) ExporterOpt {
	return func(e *Exporter) {
		e.tags = parseCSV(tags)
	}
}

// WithAutoDiscovery configures exporter with excluded database
func WithAutoDiscovery(flag bool) ExporterOpt {
	return func(e *Exporter) {
		e.autoDiscovery = flag
	}
}

// WithExcludeDatabase configures exporter with excluded database
func WithExcludeDatabase(excludeStr string) ExporterOpt {
	return func(e *Exporter) {
		exclMap := make(map[string]bool)
		exclList := parseCSV(excludeStr)
		for _, item := range exclList {
			exclMap[item] = true
		}
		e.excludeDatabase = exclMap
	}
}

// WithIncludeDatabase configures exporter with included database
func WithIncludeDatabase(includeStr string) ExporterOpt {
	return func(e *Exporter) {
		inclMap := make(map[string]bool)
		inclList := parseCSV(includeStr)
		for _, item := range inclList {
			inclMap[item] = true
		}
		e.includeDatabase = inclMap
	}
}

// WithConnectTimeout will specify timeout for conn pre-check.
// It's useful to increase this value when monitoring a remote instance (cross DC, cross AZ)
func WithConnectTimeout(timeout int) ExporterOpt {
	return func(e *Exporter) {
		e.connectTimeout = timeout
	}
}

/* ================ Exporter RESTAPI ================ */

func currentExporter() *Exporter {
	if target := currentExporterPt.Load(); target != nil {
		return target
	}
	ReloadLock.RLock()
	defer ReloadLock.RUnlock()
	return PgExporter
}

// ExplainFunc expose explain document
func (e *Exporter) ExplainFunc(w http.ResponseWriter, r *http.Request) {
	// The explain output is plain text. Serving it as text/plain avoids
	// browsers interpreting config content as HTML.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	target := currentExporter()
	if target == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("exporter unavailable"))
		return
	}
	_, _ = w.Write([]byte(target.Explain()))
}

// StatFunc exposes plain text runtime statistics.
func (e *Exporter) StatFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	target := currentExporter()
	if target == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("exporter unavailable"))
		return
	}
	_, _ = w.Write([]byte(target.Stat()))
}

// UpCheckFunc tells whether target instance is alive, 200 up 503 down
func (e *Exporter) UpCheckFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	target := currentExporter()
	if target == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unknown"))
		return
	}

	// Note: /up reports the latest state from the background health loop.
	// It does not actively probe the target on each HTTP request.
	state := target.healthStatus.Load()
	status := healthStatusText(state)
	if state == healthStatusPrimary || state == healthStatusReplica {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(status))
	} else {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(status))
	}
}

// PrimaryCheckFunc tells whether target instance is a primary, 200 yes 404 no 503 unknown
func (e *Exporter) PrimaryCheckFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	target := currentExporter()
	if target == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unknown"))
		return
	}

	state := target.healthStatus.Load()
	status := healthStatusText(state)
	if state == healthStatusPrimary || state == healthStatusReplica {
		if state == healthStatusReplica {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(status))
		} else {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(status))
		}
	} else {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(status))
	}
}

// ReplicaCheckFunc tells whether target instance is a replica, 200 yes 404 no 503 unknown
func (e *Exporter) ReplicaCheckFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	target := currentExporter()
	if target == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unknown"))
		return
	}

	state := target.healthStatus.Load()
	status := healthStatusText(state)
	if state == healthStatusPrimary || state == healthStatusReplica {
		if state == healthStatusReplica {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(status))
		} else {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(status))
		}
	} else {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(status))
	}
}

// VersionFunc responding current pg_exporter version
func VersionFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	payload := fmt.Sprintf("pg_exporter version %s\nrevision: %s\nbranch: %s\ngo version: %s\nbuild date: %s\ngoos: %s\ngoarch: %s",
		Version, Revision, Branch, GoVersion, BuildDate, GOOS, GOARCH)
	_, _ = w.Write([]byte(payload))
}

// TitleFunc responding a description message
func TitleFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	_, _ = w.Write([]byte(`<html><head><title>PG Exporter</title></head><body><h1>PG Exporter</h1><p><a href='` + html.EscapeString(*metricPath) + `'>Metrics</a></p></body></html>`))
}

// ReloadFunc handles reload request
func ReloadFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed"))
		return
	}
	if err := Reload(); err != nil {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(fmt.Sprintf("fail to reload: %s", err.Error())))
	} else {
		_, _ = w.Write([]byte(`server reloaded`))
	}
}
