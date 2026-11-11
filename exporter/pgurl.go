package exporter

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"unicode"

	"github.com/lib/pq"
)

// GetPGURL will retrieve, parse, modify postgres connection string
func GetPGURL() string {
	result := ProcessPGURL(RetrievePGURL())
	if result == "" {
		logFatalf("invalid PostgreSQL connection URL")
	}
	return result
}

// Do not return url.Parse errors: they can contain the complete URL and password.
func parsePGURL(input string) (*url.URL, url.Values, error) {
	u, err := url.Parse(input)
	if err != nil || (!strings.HasPrefix(input, "postgres://") && !strings.HasPrefix(input, "postgresql://")) || strings.Contains(input, "#") {
		return nil, nil, errors.New("invalid PostgreSQL URL syntax")
	}
	qs, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, nil, errors.New("invalid PostgreSQL URL query parameters")
	}
	// lib/pq converts URL parameters to key=value form, where whitespace and
	// '=' delimit keys. Reject names that it would interpret differently.
	for key := range qs {
		if key == "" || strings.ContainsRune(key, '=') || strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			return nil, nil, errors.New("invalid PostgreSQL URL query parameter name")
		}
	}
	return u, qs, nil
}

// RetrievePGURL retrieve pg target url from multiple sources according to precedence
// priority: cli-args > env  > env file path
//  1. Command Line Argument (--url -u -d)
//  2. Environment PG_EXPORTER_URL
//  3. From file specified via Environment PG_EXPORTER_URL_FILE
//  4. Default url
//
// The default URL intentionally targets local libpq defaults. This is a
// local-first behavior for on-host deployments, where pg_exporter usually
// runs on the same machine as PostgreSQL/PgBouncer.
func RetrievePGURL() (res string) {
	// command line args
	if *pgURL != "" {
		logInfof("retrieve target url %s from command line", ShadowPGURL(*pgURL))
		return *pgURL
	}
	// env PG_EXPORTER_URL
	if res = os.Getenv("PG_EXPORTER_URL"); res != "" {
		logInfof("retrieve target url %s from PG_EXPORTER_URL", ShadowPGURL(res))
		return res
	}
	// env PGURL
	if res = os.Getenv("PGURL"); res != "" {
		logInfof("retrieve target url %s from PGURL", ShadowPGURL(res))
		return res
	}
	// file content from file PG_EXPORTER_URL_FILE
	if filename := os.Getenv("PG_EXPORTER_URL_FILE"); filename != "" {
		if fileContents, err := os.ReadFile(filename); err != nil {
			logFatalf("PG_EXPORTER_URL_FILE=%s is specified, fail loading url: %s", filename, err.Error())
		} else {
			res = strings.TrimSpace(string(fileContents))
			logInfof("retrieve target url %s from PG_EXPORTER_URL_FILE", ShadowPGURL(res))
			return res
		}
	}
	// DEFAULT
	logWarnf("fail retrieving target url, fallback on default url: %s", defaultPGURL)
	return defaultPGURL
}

// ensureURLAuthorityPath preserves the "//" authority prefix when serializing a
// hostless, userless, pathless PostgreSQL URI (e.g. "postgresql://?host=/tmp").
// Go's net/url omits "//" when Host, User, and Path are all empty, producing
// "postgresql:?...", which lib/pq rejects. Setting Path="/" yields
// "postgresql:///?..." while keeping Database="" in lib/pq.
func ensureURLAuthorityPath(u *url.URL) {
	if u.Host == "" && u.User == nil && u.Path == "" {
		u.Path = "/"
	}
}

// ProcessPGURL will fix URL with default options.
//
// Design decision:
// If sslmode is omitted, force sslmode=disable. pg_exporter is typically
// deployed as an on-host/local exporter, where TLS on loopback adds overhead
// without meaningful security benefit. Users can always override by passing an
// explicit sslmode in the URL.
func ProcessPGURL(pgurl string) string {
	u, qs, err := parsePGURL(pgurl)
	if err != nil {
		logErrorf("%s", err)
		return ""
	}

	// add sslmode = disable if not exists
	if sslmode := qs.Get(`sslmode`); sslmode == "" {
		qs.Set(`sslmode`, `disable`)
	}
	ensureURLAuthorityPath(u)
	u.RawQuery = qs.Encode()
	return u.String()
}

// ShadowPGURL will hide password part of dsn
func ShadowPGURL(pgurl string) string {
	parsedURL, qs, err := parsePGURL(pgurl)
	if err != nil {
		return "<invalid PostgreSQL URL>"
	}

	// We need to handle two cases:
	// 1. The password is in the format postgresql://localhost:5432/postgres?sslmode=disable&user=<user>&password=<pass>
	// 2. The password is in the format postgresql://<user>:<pass>@localhost:5432/postgres?sslmode=disable

	for k, values := range qs {
		if strings.EqualFold(k, "password") || strings.EqualFold(k, "sslpassword") {
			for i := range values {
				values[i] = "xxxxx"
			}
			qs[k] = values
		}
	}
	ensureURLAuthorityPath(parsedURL)
	parsedURL.RawQuery = qs.Encode()
	return parsedURL.Redacted()
}

// ParseDatname extract database name part of a pgurl
func ParseDatname(pgurl string) string {
	cfg, err := pq.NewConfig(pgurl)
	if err != nil {
		return ""
	}
	return cfg.Database
}

// ReplaceDatname will replace pgurl with new database name
func ReplaceDatname(pgurl, datname string) string {
	u, qs, err := parsePGURL(pgurl)
	if err != nil {
		logErrorf("%s", err)
		return ""
	}
	// Conflicting query parameters can override the path in lib/pq.
	qs.Del("dbname")
	qs.Del("database")
	qs.Del("service")
	u.RawQuery = qs.Encode()
	u.Path = "/" + datname
	return u.String()
}
