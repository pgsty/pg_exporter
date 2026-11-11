package exporter

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestDatabaseURLMatchesDriver(t *testing.T) {
	for _, input := range []string{
		"postgres://monitor@localhost/aaa?dbname=zzz",
		"postgres://monitor@localhost/zzz?dbname=aaa",
		"postgres://monitor@localhost/postgres?database=pgbouncer",
		"postgres://monitor@localhost/?dbname=%20db%20",
		"postgres://monitor@localhost/%2Fdb",
	} {
		t.Run(input, func(t *testing.T) {
			cfg, err := pq.NewConfig(input)
			if err != nil {
				t.Fatal(err)
			}
			if got := ParseDatname(input); got != cfg.Database {
				t.Errorf("ParseDatname = %q; driver database = %q", got, cfg.Database)
			}
			for _, name := range []string{"aaa", "zzz", " space /?#%数据库 "} {
				changed := ReplaceDatname(input, name)
				cfg, err := pq.NewConfig(changed)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Database != name {
					t.Errorf("ReplaceDatname(%q) connects to %q", name, cfg.Database)
				}
			}
		})
	}
}

func TestProcessPGURLRejectsMalformedInput(t *testing.T) {
	var logs bytes.Buffer
	originalLogger := Logger
	Logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { Logger = originalLogger })
	for _, input := range []string{
		"postgres://monitor:secret@localhost:bad/postgres",
		"postgres://monitor:secret@localhost/db?sslmode=verify-full%zz",
		"postgres://monitor:secret@localhost/db?password=secret;sslmode=verify-full",
		"postgres://monitor:secret@localhost/db#fragment",
		"postgres://monitor@localhost/db?password%20=secret",
		"postgres://monitor@localhost/db?sslmode%20=verify-full",
		"postgres://monitor@localhost/db?database%20=locked",
		"postgres://monitor@localhost/db?password%3Dsecret=rest",
		"postgres://monitor@localhost/db?password%E2%80%83=secret",
		"postgres://monitor@localhost/db?=secret",
		"https://monitor:secret@localhost/db", "", "dbname=postgres password=secret",
	} {
		t.Run(input, func(t *testing.T) {
			logs.Reset()
			if got := ProcessPGURL(input); got != "" {
				t.Errorf("malformed input was accepted: %q", got)
			}
			if got := ReplaceDatname(input, "app"); got != "" {
				t.Errorf("malformed input was accepted during discovery: %q", got)
			}
			if got := ShadowPGURL(input); got != "<invalid PostgreSQL URL>" {
				t.Errorf("malformed input was exposed in redacted URL: %q", got)
			}
			if strings.Contains(logs.String(), "secret") {
				t.Errorf("credentials leaked in error log: %q", logs.String())
			}
		})
	}
}

func TestProcessPGURLPreservesValidQueryParameters(t *testing.T) {
	input := "postgres://monitor@localhost/db?%70assword=p%20q%3Dx&sslmode=verify-full&my_extension.setting=some%20value"
	processed := ProcessPGURL(input)
	if processed == "" {
		t.Fatal("valid URL was rejected")
	}
	cfg, err := pq.NewConfig(processed)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "p q=x" || cfg.Database != "db" || cfg.SSLMode != "verify-full" || cfg.Runtime["my_extension.setting"] != "some value" {
		t.Fatal("valid query parameters changed during normalization")
	}
	redacted, err := url.Parse(ShadowPGURL(processed))
	if err != nil || redacted.Query().Get("password") != "xxxxx" {
		t.Fatal("encoded password parameter was not redacted")
	}
}

func TestShadowPGURLInvalidInputDoesNotLeak(t *testing.T) {
	for _, input := range []string{
		"postgres://monitor:secret@localhost:bad/db",
		"postgres://monitor:secret@localhost/db?password=secret%zz",
		"postgres://monitor:secret@localhost/db#secret",
		"dbname=postgres password=secret",
	} {
		if got := ShadowPGURL(input); strings.Contains(got, "secret") || got == input {
			t.Errorf("unsafe redaction: %q", got)
		}
	}
}

func TestHostlessPGURLPreservesAuthorityForDriver(t *testing.T) {
	for _, tc := range []struct {
		input   string
		wantDB  string
		wantHost string
	}{
		{input: "postgresql://", wantDB: ""},
		{input: "postgres://", wantDB: ""},
		{input: "postgresql://?host=/var/run/postgresql", wantDB: "", wantHost: "/var/run/postgresql"},
		{input: "postgresql://monitor@?host=/tmp", wantDB: "", wantHost: "/tmp"},
		{input: "postgresql://?dbname=pgbouncer", wantDB: "pgbouncer"},
		{input: "postgresql://?host=/tmp&sslpassword=keysecret", wantDB: "", wantHost: "/tmp"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			processed := ProcessPGURL(tc.input)
			if !strings.HasPrefix(processed, "postgres://") && !strings.HasPrefix(processed, "postgresql://") {
				t.Fatalf("ProcessPGURL(%q) lost URI authority prefix: %q", tc.input, processed)
			}
			cfg, err := pq.NewConfig(processed)
			if err != nil {
				t.Fatalf("pq.NewConfig(%q) failed: %v", processed, err)
			}
			if cfg.Database != tc.wantDB {
				t.Fatalf("Database = %q, want %q", cfg.Database, tc.wantDB)
			}
			if tc.wantHost != "" && cfg.Host != tc.wantHost {
				t.Fatalf("Host = %q, want %q", cfg.Host, tc.wantHost)
			}

			shadowed := ShadowPGURL(processed)
			if strings.Contains(shadowed, "keysecret") {
				t.Fatalf("ShadowPGURL leaked sslpassword: %q", shadowed)
			}
			if !strings.HasPrefix(shadowed, "postgres://") && !strings.HasPrefix(shadowed, "postgresql://") {
				t.Fatalf("ShadowPGURL(%q) lost URI authority prefix: %q", processed, shadowed)
			}

			replaced := ReplaceDatname(processed + "&service=prod", "appdb")
			repCfg, err := pq.NewConfig(replaced)
			if err != nil {
				t.Fatalf("pq.NewConfig(ReplaceDatname) failed: %v", err)
			}
			if repCfg.Database != "appdb" {
				t.Fatalf("ReplaceDatname Database = %q, want appdb", repCfg.Database)
			}
			if strings.Contains(replaced, "service=") {
				t.Fatalf("ReplaceDatname did not strip service parameter: %q", replaced)
			}
		})
	}
}
