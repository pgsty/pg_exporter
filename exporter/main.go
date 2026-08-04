package exporter

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
)

// validateTelemetryPath checks that path can be registered alongside all of
// pg_exporter's fixed endpoints. registerHTTPRoutes is deliberately reused so
// validation cannot drift from the routes used by the real server.
func validateTelemetryPath(path string) (err error) {
	if path == "" {
		return fmt.Errorf("web.telemetry-path must not be empty")
	}
	if path[0] != '/' {
		return fmt.Errorf("web.telemetry-path %q must start with '/'", path)
	}
	uri, parseErr := url.ParseRequestURI(path)
	if parseErr != nil || strings.ContainsAny(path, "?#") || uri.RawQuery != "" || uri.Fragment != "" {
		return fmt.Errorf("web.telemetry-path %q must be a valid URL path without query or fragment", path)
	}
	// uri.Path resolves percent-escapes, so "/%75p" and "/%7Bx%7D" are checked
	// as "/up" and "/{x}" below
	decoded := uri.Path
	if strings.ContainsAny(decoded, "{}") {
		return fmt.Errorf("web.telemetry-path %q must be a literal URL path without ServeMux wildcards", path)
	}
	// ServeMux path-cleans incoming requests, so a non-canonical pattern like
	// "//metrics" or "/a/../metrics" registers fine but can never be matched
	if cleaned := pathpkg.Clean(path); path != cleaned && path != cleaned+"/" {
		return fmt.Errorf("web.telemetry-path %q must be a canonical URL path (did you mean %q?)", path, cleaned)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid or conflicting web.telemetry-path %q: %v", path, recovered)
		}
	}()
	// Register the decoded path: the go1.21 ServeMux (still selected by
	// GODEBUG=httpmuxgo121=1 and GOPATH-mode builds without a go.mod, e.g.
	// Debian dh-golang) never unescapes patterns and only panics on exact
	// duplicates, so "/%75p" would slip past a check on the raw path
	registerHTTPRoutes(http.NewServeMux(), &Exporter{}, decoded, http.NotFoundHandler())
	return nil
}

func registerHTTPRoutes(mux *http.ServeMux, e *Exporter, telemetryPath string, telemetryHandler http.Handler) {
	mux.HandleFunc("/", TitleFunc)
	mux.HandleFunc("/version", VersionFunc)
	mux.HandleFunc("/reload", ReloadFunc)
	mux.HandleFunc("/stat", e.StatFunc)
	mux.HandleFunc("/explain", e.ExplainFunc)

	for _, path := range []string{"/up", "/read", "/health", "/liveness", "/readiness"} {
		mux.HandleFunc(path, e.UpCheckFunc)
	}
	for _, path := range []string{"/primary", "/leader", "/master", "/read-write", "/rw"} {
		mux.HandleFunc(path, e.PrimaryCheckFunc)
	}
	for _, path := range []string{"/replica", "/standby", "/slave", "/read-only", "/ro"} {
		mux.HandleFunc(path, e.ReplicaCheckFunc)
	}

	mux.Handle(telemetryPath, telemetryHandler)
}

// clearLibPQEnvironment removes ambient PostgreSQL settings that pg_exporter
// must not pass to lib/pq. In particular, lib/pq supports PGSERVICE and
// PGSERVICEFILE, but service-file values can override the explicit connection
// URL selected by pg_exporter. Ignoring them keeps the advertised URL and the
// actual connection target deterministic.
func clearLibPQEnvironment() {
	variables := []struct {
		name   string
		reason string
	}{
		{"PGSYSCONFDIR", "kept isolated for compatibility with older lib/pq"},
		{"PGLOCALEDIR", "kept isolated for compatibility with older lib/pq"},
		{"PGREALM", "rejected by lib/pq"},
		{"PGSERVICEFILE", "may override the explicit pg_exporter URL"},
		{"PGSERVICE", "may override the explicit pg_exporter URL"},
	}

	for _, variable := range variables {
		if _, exists := os.LookupEnv(variable.name); !exists {
			continue
		}
		logWarnf("clearing environment variable %s (%s)", variable.name, variable.reason)
		if err := os.Unsetenv(variable.name); err != nil {
			logWarnf("failed to clear environment variable %s: %v", variable.name, err)
		}
	}
}

// DryRun will explain all query fetched from configs
func DryRun() {
	configs, err := LoadConfig(*configPath)
	if err != nil {
		logErrorf("fail loading config %s, %v", *configPath, err)
		os.Exit(1)
	}

	var queries []*Query
	for _, query := range configs {
		queries = append(queries, query)
	}
	sort.Slice(queries, func(i, j int) bool {
		return queries[i].Priority < queries[j].Priority
	})
	for _, query := range queries {
		fmt.Println(query.Explain())
	}
	fmt.Println()
	os.Exit(0)

}

// Reload will launch a new pg exporter instance
func Reload() error {
	ReloadLock.Lock()
	defer ReloadLock.Unlock()
	logDebugf("reload request received, reloading configuration")

	if *configPath == "" {
		return fmt.Errorf("no valid config path")
	}
	queries, err := LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("fail loading config %s: %w", *configPath, err)
	}

	target := PgExporter
	if target == nil {
		return fmt.Errorf("exporter unavailable")
	}

	if err := validateConstLabelConflicts(target.constLabels, queries, target.disableIntro); err != nil {
		return fmt.Errorf("invalid configuration with current constant labels: %w", err)
	}

	// Block scrapes while we swap the query set and invalidate plans.
	target.lock.Lock()
	defer target.lock.Unlock()

	target.queries = queries

	// Update queries for primary + discovered servers, and force re-plan on next scrape.
	servers := target.IterateServer()
	if target.server != nil {
		servers = append(servers, target.server)
	}
	for _, s := range servers {
		if s == nil {
			continue
		}
		s.lock.Lock()
		s.queries = queries
		s.Collectors = nil
		s.Planned = false
		s.ResetStats()
		s.lock.Unlock()
	}

	logInfof("server reloaded, %d queries applied", len(queries))
	return nil
}

// Run pg_exporter
func Run() {
	ParseArgs()

	clearLibPQEnvironment()

	// explain config only
	if *dryRun {
		DryRun()
	}

	if *configPath == "" {
		Logger.Error("no valid config path, exit")
		os.Exit(1)
	}

	if len(*webConfig.WebListenAddresses) == 0 {
		Logger.Error("invalid listen address", "addresses", *webConfig.WebListenAddresses)
		os.Exit(1)
	}
	listenAddr := (*webConfig.WebListenAddresses)[0]
	if err := validateTelemetryPath(*metricPath); err != nil {
		logErrorf("%v", err)
		os.Exit(1)
	}

	// Create exporter. It will connect on scrape and keep health probes running in background.
	var err error
	newExporter, err := NewExporter(
		*pgURL,
		WithConfig(*configPath),
		WithConstLabels(*constLabels),
		WithCacheDisabled(*disableCache),
		WithIntroDisabled(*disableIntro),
		WithFailFast(*failFast),
		WithNamespace(*exporterNamespace),
		WithAutoDiscovery(*autoDiscovery),
		WithExcludeDatabase(*excludeDatabase),
		WithIncludeDatabase(*includeDatabase),
		WithTags(*serverTags),
		WithConnectTimeout(*connectTimeout),
	)
	if err != nil {
		logErrorf("fail creating pg_exporter: %s", err.Error())
		os.Exit(2)
	}
	setCurrentExporter(newExporter)

	// trigger a manual planning before explain
	if *explainOnly {
		PgExporter.server.Plan()
		fmt.Println(PgExporter.Explain())
		os.Exit(0)
	}

	prometheus.MustRegister(PgExporter)
	defer PgExporter.Close()

	// reload conf when receiving configured reload signals (SIGHUP, and SIGUSR1 on non-Windows)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, reloadSignals...)
	go func() {
		for sig := range sigs {
			logInfof("%v received, reloading", sig)
			if err := Reload(); err != nil {
				logErrorf("reload failed: %s", err.Error())
			}
		}
	}()

	/* ================ REST API ================ */
	mux := http.NewServeMux()
	registerHTTPRoutes(mux, PgExporter, *metricPath, promhttp.Handler())

	logInfof("pg_exporter for %s start, listen on %s%s", ShadowPGURL(*pgURL), listenAddr, *metricPath)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := web.ListenAndServe(srv, webConfig, Logger); err != nil {
		logFatalf("http server failed: %s", err.Error())
	}

}
