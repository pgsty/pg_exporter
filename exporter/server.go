package exporter

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
)

/* ================ Const ================ */
const connMaxLifeTime = 1 * time.Minute // close connection after 1 minute to avoid conn leak

const pgSQLStateCannotConnectNow = "57P03"

/* ================ Server ================ */

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Server represent a postgres connection, with additional fact, conf, runtime info
type Server struct {
	*sql.DB              // database instance (do not close this due to the stupid implementation in database/sql)
	dsn     string       // data source name
	lock    sync.RWMutex // server scrape lock
	dbMu    sync.RWMutex // DB initialization only; never held during SQL
	err     error        // last error
	// interrupted reports that the last pass ended because the request was
	// canceled or ran out of budget, which says nothing about the database.
	interrupted bool

	// notice handling (primarily for pgbouncer, where SHOW VERSION may return via NOTICE)
	noticeMu   sync.Mutex
	lastNotice string

	// hooks
	beforeScrape     func(context.Context, *Server) error // hook: execute before scrape
	onDatabaseChange func(change map[string]bool)         // hook: invoke when database list is changed

	// postgres fact gather from server
	UP       bool   // indicate whether target server is connectable
	Recovery bool   // is server in recovering
	Version  int    // pg server version num
	Database string // database name of current server connection
	Username string // current username

	Databases  map[string]bool // all available database in target cluster
	dblistLock sync.Mutex      // lock when access Databases map

	Namespaces map[string]bool // all available schema in target cluster
	Extensions map[string]bool // all available extension in target cluster

	Tags            []string // server tags set by cli arg --tag
	PgbouncerMode   bool     // indicate it is a pgbouncer server
	DisableCache    bool     // force executing, ignoring caching policy
	ExcludeDbnames  []string // if ExcludeDbnames is provided, Auto Database Discovery is enabled
	Forked          bool     // is this a forked server ? (does not run cluster level query)
	Planned         bool     // if false, server will trigger a plan before collect
	MaxConn         int      // max connection for this server
	ConnectTimeout  int      // connect timeout for this server in ms
	ConnMaxLifetime int      // connection max lifetime for this server in seconds

	// query
	Collectors []*Collector      // query collector instance (installed query)
	queries    map[string]*Query // queries map, keys are config file top layer key
	labels     prometheus.Labels // constant labels

	// internal stats
	serverInit  time.Time // server init timestamp
	scrapeBegin time.Time // server last scrape begin time
	scrapeDone  time.Time // server last scrape done time
	errorCount  float64   // total scrape error count on this server (fatal scrape failures only)
	totalCount  float64   // total scrape count on this server
	totalTime   float64   // total time spend on scraping

	queryCacheTTL                 map[string]float64 // internal query metrics: cache time to live
	queryScrapeTotalCount         map[string]float64 // internal query metrics: total executed
	queryScrapeHitCount           map[string]float64 // internal query metrics: times serving from hit cache
	queryScrapeErrorCount         map[string]float64 // internal query metrics: times failed
	queryScrapePredicateSkipCount map[string]float64 // internal query metrics: times skipped due to predicate
	queryScrapeMetricCount        map[string]float64 // internal query metrics: number of metrics scraped
	queryScrapeDuration           map[string]float64 // internal query metrics: time spend on executing
}

func (s *Server) GetConnectTimeout() time.Duration {
	if s.ConnectTimeout <= 0 {
		return 100 * time.Millisecond
	}
	return time.Duration(s.ConnectTimeout) * time.Millisecond
}

// Name is coalesce(s.Database, dsn)
func (s *Server) Name() string {
	if s.Database != "" {
		return s.Database
	}
	return ShadowPGURL(s.dsn)
}

func (s *Server) Error() error {
	return s.err
}

// Check will issue a connection and executing precheck hook function
func (s *Server) Check() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.beforeScrape(context.Background(), s)
}

// ProbeHealth performs a lightweight probe for exporter health checks.
// It returns whether the database is reachable, whether it's in recovery,
// and whether PostgreSQL is still starting up (SQLSTATE 57P03).
func (s *Server) ProbeHealth() (up, recovery, starting bool, err error) {
	// DB initialization has its own short lock. A scrape must not delay probes.
	// Mode and timeout are immutable after construction.
	s.dbMu.RLock()
	db := s.DB
	s.dbMu.RUnlock()
	pgbouncerMode := s.PgbouncerMode
	timeout := s.GetConnectTimeout()

	if db == nil {
		return false, false, false, errors.New("database connection is not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if pgbouncerMode {
		// IMPORTANT: do NOT use db.PingContext() for PgBouncer.
		//
		// lib/pq implements Ping() by sending a simpleQuery(";") (empty command).
		// PostgreSQL tolerates that, but PgBouncer rejects it with:
		//   invalid command ';', use SHOW HELP;
		// This becomes a log-spam regression once ProbeHealth is run periodically.
		//
		// For health probes we only need a cheap PgBouncer admin command.
		// Some PgBouncer versions return SHOW VERSION via NOTICE (no result rows),
		// so treat sql.ErrNoRows as success.
		var dummy string
		qerr := db.QueryRowContext(ctx, `SHOW VERSION;`).Scan(&dummy)
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			return false, false, false, qerr
		}
		return true, false, false, nil
	}

	if err = db.QueryRowContext(ctx, `SELECT pg_catalog.pg_is_in_recovery();`).Scan(&recovery); err != nil {
		starting = isPostgresStartupError(err)
		return false, false, starting, err
	}

	return true, recovery, false, nil
}

func isPostgresStartupError(err error) bool {
	var pgErr *pq.Error
	return errors.As(err, &pgErr) && string(pgErr.Code) == pgSQLStateCannotConnectNow
}

// initDB is called under the scrape lock. sql.OpenDB does not connect to the target.
func (s *Server) initDB() error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if s.DB != nil {
		return nil
	}
	base, err := pq.NewConnector(s.dsn)
	if err != nil {
		return err
	}
	var connector driver.Connector = base
	s.MaxConn = 3
	if s.Forked {
		s.MaxConn = 1
	} else if s.PgbouncerMode {
		s.MaxConn = 2 // one collection and one independent health probe
	}
	if s.PgbouncerMode {
		connector = pq.ConnectorWithNoticeHandler(base, func(notice *pq.Error) {
			s.noticeMu.Lock()
			s.lastNotice = notice.Message
			s.noticeMu.Unlock()
		})
	}
	s.DB = sql.OpenDB(connector)
	s.DB.SetMaxIdleConns(s.MaxConn)
	s.DB.SetMaxOpenConns(s.MaxConn)
	s.DB.SetConnMaxLifetime(connMaxLifeTime)
	return nil
}

// PgbouncerPrecheck checks pgbouncer connection before scrape.
func PgbouncerPrecheck(s *Server) error {
	return pgbouncerPrecheck(context.Background(), s)
}

func pgbouncerPrecheck(parent context.Context, s *Server) (err error) {
	if err = s.initDB(); err != nil {
		s.UP = false
		return err
	}

	// Clear last notice before issuing SHOW VERSION so we can reliably parse it.
	s.noticeMu.Lock()
	s.lastNotice = ""
	s.noticeMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, s.GetConnectTimeout())
	defer cancel()

	var versionStr string
	qerr := s.DB.QueryRowContext(ctx, `SHOW VERSION;`).Scan(&versionStr)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		// Connection/auth errors should fail precheck (and thus scrape).
		s.UP = false
		return fmt.Errorf("fail fetching pgbouncer version: %w", qerr)
	}

	s.UP = true

	// Version may come from:
	// 1) a normal 1-row resultset (older versions / some builds), or
	// 2) a server NOTICE message (pgbouncer 1.12+).
	newVer := 0
	if qerr == nil {
		newVer = ParseSemver(versionStr)
	}
	if newVer == 0 {
		s.noticeMu.Lock()
		notice := s.lastNotice
		s.noticeMu.Unlock()
		if notice != "" {
			newVer = ParseSemver(notice)
		}
	}

	if newVer != 0 {
		if s.Version != newVer {
			logInfof("server [%s] pgbouncer version changed: from [%d] to [%d]", s.Name(), s.Version, newVer)
			s.Planned = false
		}
		s.Version = newVer
	} else if qerr == nil || errors.Is(qerr, sql.ErrNoRows) {
		// Connected, but couldn't parse version string.
		logWarnf("server [%s] connected but failed to parse pgbouncer version (row=%q)", s.Name(), versionStr)
	}

	return nil
}

// ParseSemver will turn semantic version string into integer
func ParseSemver(semverStr string) int {
	semver := semverRe.FindStringSubmatch(semverStr)
	logDebugf("parse pgbouncer semver string %s", semverStr)
	if len(semver) != 4 {
		return 0
	}
	verNum := 0
	if major, err := strconv.Atoi(semver[1]); err != nil {
		return 0
	} else {
		verNum += major * 10000
	}
	if minor, err := strconv.Atoi(semver[2]); err != nil {
		return 0
	} else {
		verNum += minor * 100
	}
	if release, err := strconv.Atoi(semver[3]); err != nil {
		return 0
	} else {
		verNum += release
	}
	return verNum
}

// PostgresPrecheck checks postgres connection and gathering facts
// if any important fact changed, it will trigger a plan before next scrape
func PostgresPrecheck(s *Server) error {
	return postgresPrecheck(context.Background(), s)
}

func postgresPrecheck(parent context.Context, s *Server) (err error) {
	if err = s.initDB(); err != nil {
		s.UP = false
		return err
	}

	// retrieve version info
	var version int
	ctx, cancel := context.WithTimeout(parent, s.GetConnectTimeout())
	defer cancel()
	if err = s.DB.QueryRowContext(ctx, `SHOW server_version_num;`).Scan(&version); err != nil {
		s.UP = false
		return fmt.Errorf("fail fetching server version: %w", err)
	}
	s.UP = true
	// fact change triggers a new planning
	if s.Version != version {
		logInfof("server [%s] version changed: from [%d] to [%d]", s.Name(), s.Version, version)
		s.Planned = false
	}
	s.Version = version

	ctxSet, cancelSet := context.WithTimeout(parent, s.GetConnectTimeout())
	defer cancelSet()
	if _, err = s.DB.ExecContext(ctxSet, `SET application_name = pg_exporter;`); err != nil {
		s.UP = false
		return fmt.Errorf("fail setting application name: %w", err)
	}

	// get important metadata
	var recovery bool
	var datname, username string
	var databases, namespaces, extensions []string
	precheckSQL := `SELECT current_catalog, current_user, pg_catalog.pg_is_in_recovery(),
	(SELECT pg_catalog.array_agg(d.datname)::text[] AS databases FROM pg_catalog.pg_database d WHERE d.datallowconn AND NOT d.datistemplate),
	(SELECT pg_catalog.array_agg(n.nspname)::text[] AS namespaces FROM pg_catalog.pg_namespace n),
	(SELECT pg_catalog.array_agg(e.extname)::text[] AS extensions FROM pg_catalog.pg_extension e);`
	ctx2, cancel2 := context.WithTimeout(parent, s.GetConnectTimeout())
	defer cancel2()
	if err = s.DB.QueryRowContext(ctx2, precheckSQL).Scan(&datname, &username, &recovery, pq.Array(&databases), pq.Array(&namespaces), pq.Array(&extensions)); err != nil {
		s.UP = false
		return fmt.Errorf("fail fetching server version: %w", err)
	}
	if s.Recovery != recovery {
		logInfof("server [%s] recovery status changed: from [%v] to [%v]", s.Name(), s.Recovery, recovery)
		s.Planned = false
	}
	s.Recovery = recovery
	if s.Username != username {
		s.Planned = false
	}
	s.Username = username
	if s.Database != datname {
		logInfof("server [%s] datname changed: from [%s] to [%s]", s.Name(), s.Database, datname)
		s.Planned = false
	}
	s.Database = datname
	s.Databases[datname] = true

	// update schema & extension list
	newNamespaces := make(map[string]bool, len(namespaces))
	for _, nsname := range namespaces {
		newNamespaces[nsname] = true
	}
	newExtensions := make(map[string]bool, len(extensions))
	for _, extname := range extensions {
		newExtensions[extname] = true
	}
	if !maps.Equal(s.Namespaces, newNamespaces) || !maps.Equal(s.Extensions, newExtensions) {
		s.Planned = false
	}
	s.Namespaces, s.Extensions = newNamespaces, newExtensions

	// detect db change
	s.dblistLock.Lock()
	defer s.dblistLock.Unlock()
	newDBList := make(map[string]bool, len(databases))
	changes := make(map[string]bool)
	// if new db is not found in old db list, add a change entry [NewDBName:true]
	for _, dbname := range databases {
		newDBList[dbname] = true
		if _, found := s.Databases[dbname]; !found {
			logDebugf("server [%s] found new database %s", s.Name(), dbname)
			changes[dbname] = true
		}
	}
	// if old db is not found in new db list, add a change entry [OldDBName:false]
	for dbname := range s.Databases {
		if _, found := newDBList[dbname]; !found {
			logDebugf("server [%s] found vanished database %s", s.Name(), dbname)
			changes[dbname] = false
		}
	}
	// invoke hook if there are changes on database list
	if len(changes) > 0 && s.onDatabaseChange != nil {
		logDebugf("server [%s] auto discovery database list change : %v", s.Name(), changes)
		s.onDatabaseChange(changes) // if doing something long, launch another goroutine
	}
	s.Databases = newDBList
	return nil
}

// Plan will install queries that compatible with server fact (version, level, recovery, plugin, tags,...)
func (s *Server) Plan(queries ...*Query) {
	// if queries are explicitly given, use it instead of server.queries
	if len(queries) > 0 {
		newQueries := make(map[string]*Query)
		for _, q := range queries {
			newQueries[q.Name] = q
		}
		s.queries = newQueries
	}

	// check query compatibility
	instances := make([]*Collector, 0)
	var installedNames, discardedNames []string
	for name, query := range s.queries {
		if ok, reason := s.Compatible(query); ok {
			instances = append(instances, NewCollector(query, s))
			installedNames = append(installedNames, query.Branch)
		} else {
			discardedNames = append(discardedNames, query.Branch)
			logDebugf("query [%s].%s discarded because of %s", query.Name, name, reason)
		}
	}

	// sort by priority
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].Priority < instances[j].Priority
	})
	s.Collectors = instances

	// reset statistics after planning
	s.ResetStats()
	s.Planned = true
	logInfof("server [%s] planned with %d queries, %d installed, %d discarded, installed: %s , discarded: %s",
		s.Name(), len(s.queries), len(installedNames), len(discardedNames), strings.Join(installedNames, ", "), strings.Join(discardedNames, ", "))
}

// ResetStats will clear all statistic info
func (s *Server) ResetStats() {
	n := len(s.Collectors)
	s.queryCacheTTL = make(map[string]float64, n)
	s.queryScrapeTotalCount = make(map[string]float64, n)
	s.queryScrapeHitCount = make(map[string]float64, n)
	s.queryScrapeErrorCount = make(map[string]float64, n)
	s.queryScrapePredicateSkipCount = make(map[string]float64, n)
	s.queryScrapeMetricCount = make(map[string]float64, n)
	s.queryScrapeDuration = make(map[string]float64, n)

	for _, query := range s.Collectors {
		s.queryCacheTTL[query.Name] = 0
		s.queryScrapeTotalCount[query.Name] = 0
		s.queryScrapeHitCount[query.Name] = 0
		s.queryScrapeErrorCount[query.Name] = 0
		if len(query.PredicateQueries) > 0 {
			s.queryScrapePredicateSkipCount[query.Name] = 0
		}
		s.queryScrapeMetricCount[query.Name] = 0
		s.queryScrapeDuration[query.Name] = 0
	}
}

// Compatible tells whether a query is compatible with current server
func (s *Server) Compatible(query *Query) (res bool, reason string) {
	// check skip flag
	if query.Skip {
		return false, fmt.Sprintf("query %s is marked skip", query.Name)
	}

	// check mode
	if pgbouncerQuery := query.HasTag("pgbouncer"); pgbouncerQuery != s.PgbouncerMode {
		if s.PgbouncerMode {
			return false, fmt.Sprintf("pgbouncer server doese not match with normal postgres query %s", query.Name)
		}
		return false, fmt.Sprintf("pgbouncer query %s does not match with normal postgres server", query.Name)
	}

	// check version
	if s.Version != 0 { // if version is not determined yet, just let it go
		if query.MinVersion != 0 && s.Version < query.MinVersion {
			return false, fmt.Sprintf("server version %v lower than query min version %v", s.Version, query.MinVersion)
		}
		if query.MaxVersion != 0 && s.Version >= query.MaxVersion { // exclude
			return false, fmt.Sprintf("server version %v higher than query max version %v", s.Version, query.MaxVersion)
		}
	}

	// check query side tags
	for _, tag := range query.Tags {
		// check extension is installed on target database
		if strings.HasPrefix(tag, "extension:") {
			if _, found := s.Extensions[strings.TrimPrefix(tag, "extension:")]; !found {
				return false, fmt.Sprintf("server [%s] does not have extension %s", s.Name(), tag)
			}
			continue
		}

		// check schema exist on target database
		if strings.HasPrefix(tag, "schema:") {
			if _, found := s.Namespaces[strings.TrimPrefix(tag, "schema:")]; !found {
				return false, fmt.Sprintf("server [%s] does not have schema %s", s.Name(), tag)
			}
			continue
		}

		// check if dbname prefix tag match server.Database
		if strings.HasPrefix(tag, "dbname:") {
			if s.Database != strings.TrimPrefix(tag, "dbname:") {
				return false, fmt.Sprintf("server [%s] dbname does %s not match with query tag %s", s.Name(), s.Database, tag)
			}
			continue
		}

		// check if username prefix tag match server.Username
		if strings.HasPrefix(tag, "username:") {
			if s.Username != strings.TrimPrefix(tag, "username:") {
				return false, fmt.Sprintf("server [%s] username [%s] does not match %s", s.Name(), s.Username, tag)
			}
			continue
		}

		// check server does not have given tag
		if strings.HasPrefix(tag, "not:") {
			if negTag := strings.TrimPrefix(tag, "not:"); s.HasTag(negTag) {
				return false, fmt.Sprintf("server [%s] has tag %s that query %s forbid", s.Name(), negTag, query.Name)
			}
			continue
		}

		// check 3 default tags: cluster, primary, standby|replica
		switch tag {
		case "cluster":
			if s.Forked {
				return false, fmt.Sprintf("cluster level query %s will not run on forked server %v", query.Name, s.Name())
			}
			continue
		case "primary", "master", "leader":
			if s.Recovery {
				return false, fmt.Sprintf("primary-only query %s will not run on standby server %v", query.Name, s.Name())
			}
			continue
		case "standby", "replica", "slave":
			if !s.Recovery {
				return false, fmt.Sprintf("standby-only query %s will not run on primary server %v", query.Name, s.Name())
			}
			continue
		case "pgbouncer":
			continue
		default:
			// if this tag is nether a pre-defined tag nor a prefixed pattern tag, check whether server have that tag
			if !s.HasTag(tag) {
				return false, fmt.Sprintf("server [%s] does not have tag %s that query %s require", s.Name(), tag, query.Name)
			}
		}
	}
	return true, ""
}

// Explain will print all queries that registered to server
func (s *Server) Explain() string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var res []string
	for _, i := range s.Collectors {
		res = append(res, i.Explain())
	}
	return strings.Join(res, "\n")
}

// Stat will turn Server internal stats into HTML
func (s *Server) Stat() string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	buf := new(bytes.Buffer)
	buf.WriteString(fmt.Sprintf("%-24s %-10s %-10s %-10s %-10s %-10s %-6s %-10s\n", "name", "total", "hit", "error", "skip", "metric", "ttl/s", "duration/ms"))
	for _, query := range s.Collectors {
		buf.WriteString(fmt.Sprintf("%-24s %-10d %-10d %-10d %-10d %-10d %-6d %-10f\n",
			query.Name,
			int(s.queryScrapeTotalCount[query.Name]),
			int(s.queryScrapeHitCount[query.Name]),
			int(s.queryScrapeErrorCount[query.Name]),
			int(s.queryScrapePredicateSkipCount[query.Name]),
			int(s.queryScrapeMetricCount[query.Name]),
			int(s.queryCacheTTL[query.Name]),
			s.queryScrapeDuration[query.Name]*1000,
		))
	}
	return buf.String()
}

// ExplainHTML will print server stats in HTML format
func (s *Server) ExplainHTML() string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var res []string
	for _, i := range s.Collectors {
		res = append(res, i.HTML())
	}
	return strings.Join(res, "</br></br>")
}

// Describe implement prometheus.Collector
func (s *Server) Describe(ch chan<- *prometheus.Desc) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	for _, instance := range s.Collectors {
		instance.Describe(ch)
	}
}

// Collect implement prometheus.Collector interface
func (s *Server) Collect(ch chan<- prometheus.Metric) {
	s.collect(context.Background(), ch)
}

func (s *Server) collect(ctx context.Context, ch chan<- prometheus.Metric) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.scrapeBegin = time.Now() // This ts is used for cache expiration check
	wasUP := s.UP

	// check server conn, gathering fact
	if s.err = ctx.Err(); s.err != nil {
		goto final
	}
	if s.err = s.beforeScrape(ctx, s); s.err != nil {
		logDebugf("fail establishing connection to %s: %s", s.Name(), s.err.Error())
		goto final
	}
	wasUP = true // the database answered this pass

	// fact change (including first time) will incur a plan procedure
	if !s.Planned {
		s.Plan()
	}

	// First pass: execute all queries with Fatal flag
	if err := s.collectFatalQueries(ctx, ch); err != nil {
		s.err = err
		goto final
	}

	// Second pass: execute remaining non-Fatal queries
	s.collectNonFatalQueries(ctx, ch)
	if ctx.Err() != nil {
		s.err = ctx.Err()
	}

final:
	s.scrapeDone = time.Now() // This ts is used for cache expiration check
	s.totalTime += s.scrapeDone.Sub(s.scrapeBegin).Seconds()
	s.totalCount++
	s.interrupted = s.err != nil && ctx.Err() != nil
	if s.interrupted {
		// The client left or the request budget expired. Keep the last database
		// observation and leave connection/fatal error counters unchanged.
		s.UP = wasUP
		logInfof("scrape of server [%s] interrupted: %s", s.Name(), s.err.Error())
	} else if s.err != nil {
		s.UP = false
		s.errorCount++
		logErrorf("fail scraping server [%s]: %s", s.Name(), s.err.Error())
	} else {
		s.UP = true
		logDebugf("server [%s] scraped in %v",
			s.Name(), s.scrapeDone.Sub(s.scrapeBegin).Seconds())
	}
}

// collectFatalQueries executes all queries with Fatal flag and returns on first error
func (s *Server) collectFatalQueries(ctx context.Context, ch chan<- prometheus.Metric) error {
	for _, query := range s.Collectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !query.Fatal {
			continue
		}

		if err := s.executeQuery(ctx, query, ch); err != nil {
			logErrorf("query [%s] error: %s", query.Name, err)
			return err
		}
	}
	return nil
}

// collectNonFatalQueries executes all non-Fatal queries and logs errors without stopping
func (s *Server) collectNonFatalQueries(ctx context.Context, ch chan<- prometheus.Metric) {
	for _, query := range s.Collectors {
		if ctx.Err() != nil {
			return
		}
		if query.Fatal {
			continue
		}

		if err := s.executeQuery(ctx, query, ch); err != nil {
			logWarnf("query [%s] error skipped: %s", query.Name, err)
		}
	}
}

// executeQuery runs a single query and updates its metrics
func (s *Server) executeQuery(ctx context.Context, query *Collector, ch chan<- prometheus.Metric) error {
	query.collect(ctx, ch)
	s.queryCacheTTL[query.Name] = query.cacheTTL()
	s.queryScrapeTotalCount[query.Name]++
	s.queryScrapeMetricCount[query.Name] = float64(query.ResultSize())
	s.queryScrapeDuration[query.Name] = query.scrapeDuration.Seconds()

	if query.Error() != nil {
		s.queryScrapeErrorCount[query.Name]++
		return query.Error()
	}

	if query.CacheHit() {
		s.queryScrapeHitCount[query.Name]++
	}

	// Update predicate skip count if applicable
	if len(query.PredicateQueries) > 0 {
		skipped, _ := query.PredicateSkip()
		if skipped {
			s.queryScrapePredicateSkipCount[query.Name]++
		}
	}

	return nil
}

// HasTag tells whether this server have specific tag
func (s *Server) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Duration returns last scrape duration in float64 seconds
func (s *Server) Duration() float64 {
	s.lock.RLock()
	defer s.lock.RUnlock()
	sec := s.scrapeDone.Sub(s.scrapeBegin).Seconds()
	return sec
}

// Uptime returns servers's uptime
func (s *Server) Uptime() float64 {
	return time.Since(s.serverInit).Seconds()
}

/* ================ Server Creation ================ */

// NewServer will check dsn, but not trying to connect
func NewServer(dsn string, opts ...ServerOpt) *Server {
	s := &Server{dsn: dsn}
	for _, opt := range opts {
		opt(s)
	}
	s.Database = ParseDatname(dsn)
	if s.Database != "pgbouncer" {
		s.PgbouncerMode = false
		s.beforeScrape = postgresPrecheck
	} else {
		logInfof("datname pgbouncer detected, enabling pgbouncer mode")
		s.PgbouncerMode = true
		s.beforeScrape = pgbouncerPrecheck
	}
	s.MaxConn = 1
	s.Databases = make(map[string]bool, 1)
	s.serverInit = time.Now()
	return s
}

// ServerOpt configures Server
type ServerOpt func(*Server)

// WithConstLabel copy constant label map to server
func WithConstLabel(labels prometheus.Labels) ServerOpt {
	return func(s *Server) {
		if labels == nil {
			s.labels = nil
		} else {
			s.labels = make(prometheus.Labels, len(labels))
			for k, v := range labels {
				s.labels[k] = v
			}
		}
	}
}

// WithCachePolicy will pass cache option to server
func WithCachePolicy(disableCache bool) ServerOpt {
	return func(s *Server) {
		s.DisableCache = disableCache
	}
}

// WithQueries set server's default query set
func WithQueries(queries map[string]*Query) ServerOpt {
	return func(s *Server) {
		s.queries = queries
	}
}

// WithServerTags will mark server only execute query without cluster tag
func WithServerTags(tags []string) ServerOpt {
	return func(s *Server) {
		s.Tags = tags
	}
}

// WithServerConnectTimeout will set a connect timeout for server precheck queries
// otherwise, a default value 100ms will be used.
// Increase this value if you are monitoring a remote (cross-DC, cross-AZ) instance
func WithServerConnectTimeout(timeout int) ServerOpt {
	return func(s *Server) {
		s.ConnectTimeout = timeout
	}
}
