package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExporterOptionHelpers(t *testing.T) {
	e := &Exporter{}

	WithConfig("/tmp/c.yml")(e)
	WithConstLabels("k=v,env=prod")(e)
	WithCacheDisabled(true)(e)
	WithIntroDisabled(true)(e)
	WithFailFast(true)(e)
	WithNamespace("custom")(e)
	WithTags("a,b")(e)
	WithAutoDiscovery(true)(e)
	WithExcludeDatabase("template0,template1")(e)
	WithIncludeDatabase("app,metrics")(e)
	WithConnectTimeout(500)(e)

	if e.configPath != "/tmp/c.yml" {
		t.Fatalf("configPath = %s", e.configPath)
	}
	if e.constLabels["k"] != "v" || e.constLabels["env"] != "prod" {
		t.Fatalf("constLabels = %v", e.constLabels)
	}
	if !e.disableCache || !e.disableIntro || !e.failFast || !e.autoDiscovery {
		t.Fatalf("boolean options not applied: cache=%v intro=%v failFast=%v auto=%v", e.disableCache, e.disableIntro, e.failFast, e.autoDiscovery)
	}
	if e.namespace != "custom" {
		t.Fatalf("namespace = %s", e.namespace)
	}
	if len(e.tags) != 2 || e.tags[0] != "a" || e.tags[1] != "b" {
		t.Fatalf("tags = %#v", e.tags)
	}
	if !e.excludeDatabase["template0"] || !e.excludeDatabase["template1"] {
		t.Fatalf("excludeDatabase = %v", e.excludeDatabase)
	}
	if !e.includeDatabase["app"] || !e.includeDatabase["metrics"] {
		t.Fatalf("includeDatabase = %v", e.includeDatabase)
	}
	if e.connectTimeout != 500 {
		t.Fatalf("connectTimeout = %d", e.connectTimeout)
	}
}

func TestPublicHandlers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	wTitle := httptest.NewRecorder()
	TitleFunc(wTitle, req)
	if wTitle.Code != http.StatusOK || !strings.Contains(wTitle.Body.String(), "PG Exporter") {
		t.Fatalf("TitleFunc unexpected response: code=%d body=%s", wTitle.Code, wTitle.Body.String())
	}

	wVersion := httptest.NewRecorder()
	VersionFunc(wVersion, req)
	if wVersion.Code != http.StatusOK || !strings.Contains(wVersion.Body.String(), "pg_exporter version") {
		t.Fatalf("VersionFunc unexpected response: code=%d body=%s", wVersion.Code, wVersion.Body.String())
	}
}

func TestExplainAndStatHandlersWhenExporterUnavailable(t *testing.T) {
	origin := PgExporter
	setCurrentExporter(nil)
	defer setCurrentExporter(origin)

	e := &Exporter{}
	req := httptest.NewRequest(http.MethodGet, "/explain", nil)

	wExplain := httptest.NewRecorder()
	e.ExplainFunc(wExplain, req)
	if wExplain.Code != http.StatusServiceUnavailable {
		t.Fatalf("ExplainFunc status = %d, want 503", wExplain.Code)
	}

	wStat := httptest.NewRecorder()
	e.StatFunc(wStat, req)
	if wStat.Code != http.StatusServiceUnavailable {
		t.Fatalf("StatFunc status = %d, want 503", wStat.Code)
	}
}

func TestHealthHandlersPassiveModeNoActiveProbe(t *testing.T) {
	var checkCount atomic.Int32
	s := &Server{
		Database:  "postgres",
		Databases: map[string]bool{"postgres": true},
	}
	s.beforeScrape = func(_ context.Context, s *Server) error {
		checkCount.Add(1)
		s.UP = false
		s.Recovery = false
		return nil
	}

	e := &Exporter{server: s}
	e.updateHealthState(true, false) // cached primary/up state
	origin := PgExporter
	setCurrentExporter(e)
	defer setCurrentExporter(origin)

	req := httptest.NewRequest(http.MethodGet, "/up", nil)
	w := httptest.NewRecorder()
	e.UpCheckFunc(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("passive health check should use cached up status, got %d", w.Code)
	}
	if checkCount.Load() != 0 {
		t.Fatalf("passive health check should not probe DB, count=%d", checkCount.Load())
	}
}
