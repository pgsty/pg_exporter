package exporter

import (
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
)

/* ================ Parameters ================ */

// Version is the fallback for plain go builds; make build overrides it via
// ldflags with the Makefile VERSION. Keep both v-prefixed and in sync so every
// build path reports the same string.
var Version = "v1.6.0"

// Build information. Populated at build-time.
var (
	Branch    = "main"
	Revision  = "HEAD"
	BuildDate = "20250421212100" // will be overwritten during release
	GoVersion = runtime.Version()
	GOOS      = runtime.GOOS
	GOARCH    = runtime.GOARCH
)

var defaultPGURL = "postgresql:///?sslmode=disable"

/* ================ Global Vars ================ */

// PgExporter is the global singleton of Exporter
var (
	PgExporter        *Exporter
	currentExporterPt atomic.Pointer[Exporter]
	ReloadLock        sync.RWMutex
	Logger            = slog.Default()
)

func setCurrentExporter(e *Exporter) {
	PgExporter = e
	currentExporterPt.Store(e)
}
