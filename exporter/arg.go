package exporter

import (
	"fmt"
	"runtime"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

var (
	// exporter settings
	pgURL             = kingpin.Flag("url", "postgres target url").Short('d').Short('u').String()
	configPath        = kingpin.Flag("config", "path to config dir or file").Short('c').String()
	webConfig         = kingpinflag.AddFlags(kingpin.CommandLine, ":9630")
	constLabels       = kingpin.Flag("label", "constant lables:comma separated list of label=value pair").Short('l').Default("").Envar("PG_EXPORTER_LABEL").String()
	serverTags        = kingpin.Flag("tag", "tags,comma separated list of server tag").Default("").Short('t').Envar("PG_EXPORTER_TAG").String()
	disableCache      = kingpin.Flag("disable-cache", "force not using cache").Default("false").Short('C').Envar("PG_EXPORTER_DISABLE_CACHE").Bool()
	disableIntro      = kingpin.Flag("disable-intro", "disable collector level introspection metrics").Short('m').Default("false").Envar("PG_EXPORTER_DISABLE_INTRO").Bool()
	autoDiscovery     = kingpin.Flag("auto-discovery", "automatically scrape all database for given server").Short('a').Default("true").Envar("PG_EXPORTER_AUTO_DISCOVERY").Bool()
	excludeDatabase   = kingpin.Flag("exclude-database", "excluded databases when enabling auto-discovery").Short('x').Default("template0,template1,postgres").Envar("PG_EXPORTER_EXCLUDE_DATABASE").String()
	includeDatabase   = kingpin.Flag("include-database", "included databases when enabling auto-discovery").Short('i').Default("").Envar("PG_EXPORTER_INCLUDE_DATABASE").String()
	exporterNamespace = kingpin.Flag("namespace", "prefix of built-in metrics, (pg|pgbouncer) by default").Short('n').Default("").Envar("PG_EXPORTER_NAMESPACE").String()
	failFast          = kingpin.Flag("fail-fast", "fail fast instead of waiting during start-up").Short('f').Envar("PG_EXPORTER_FAIL_FAST").Default("false").Bool()
	connectTimeout    = kingpin.Flag("connect-timeout", "connect timeout in ms, 100 by default").Short('T').Envar("PG_EXPORTER_CONNECT_TIMEOUT").Default("100").Int()

	// prometheus http
	// listenAddress = kingpin.Flag("web.listen-address", "prometheus web server listen address").Short('L').Default(":9630").Envar("PG_EXPORTER_LISTEN_ADDRESS").String()
	metricPath = kingpin.Flag("web.telemetry-path", "URL path under which to expose metrics.").Short('P').Default("/metrics").Envar("PG_EXPORTER_TELEMETRY_PATH").String()

	// action
	dryRun      = kingpin.Flag("dry-run", "dry run and print raw configs").Default("false").Short('D').Bool()
	explainOnly = kingpin.Flag("explain", "explain server planned queries").Default("false").Short('E').Bool()

	// logger setting
	logLevel  = kingpin.Flag("log.level", "log level: debug|info|warn|error]").Default("info").String()
	logFormat = kingpin.Flag("log.format", "log format: logfmt|json").Default("logfmt").String()
)

// ParseArgs will parse cli args with kingpin. url and config have special treatment
func ParseArgs() {
	kingpin.Version(fmt.Sprintf("pg_exporter %s (built with %s on %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	Logger = configureLogger(*logLevel, *logFormat)
	logDebugf("init pg_exporter, configPath=%v constLabels=%v disableCache=%v autoDiscovery=%v excludeDatabase=%v includeDatabase=%v connectTimeout=%vms webConfig=%v metricPath=%v",
		*configPath, *constLabels, *disableCache, *autoDiscovery, *excludeDatabase, *includeDatabase, *connectTimeout, *webConfig.WebListenAddresses, *metricPath)
	*pgURL = GetPGURL()
	*configPath = GetConfig()
}
