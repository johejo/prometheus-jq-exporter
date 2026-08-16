package exporter

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var (
	addr                      = flag.String("addr", ":9999", "listen addr")
	config                    = flag.String("config", "config.yaml", "config file path")
	expandEnv                 = flag.Bool("expand-env", false, "expand environment variable in config file")
	loglevel                  = flag.String("log-level", "info", "log level")
	logFormat                 = flag.String("log-format", "text", "log format (text or json)")
	exposeMetadata            = flag.Bool("expose-metadata", true, "expose metric metadata")
	enableFileTransport       = flag.Bool("enable-file-transport", false, "enable file transport")
	enableUnixSocketTransport = flag.Bool("enable-unix-socket-transport", false, "enable unix socket transport")
	maxResponseBodySize       = flag.Int64("max-response-body-size", defaultMaxResponseBodySize, "maximum target response body size in bytes")
	targetTimeout             = flag.Duration("target-timeout", defaultTargetTimeout, "target request timeout")
	readHeaderTimeout         = flag.Duration("read-header-timeout", defaultReadHeaderTimeout, "HTTP server request header read timeout")
	showVersion               = flag.Bool("version", false, "print version and exit")
)

const (
	defaultMaxResponseBodySize int64 = 10 << 20
	defaultTargetTimeout             = 30 * time.Second
	defaultReadHeaderTimeout         = 5 * time.Second

	valueTypeCounter             = "counter"
	valueTypeGauge               = "gauge"
	valueTypeUntyped             = "untyped"
	buildInfoMetric              = "jq_exporter_build_info"
	probeBodyErrorsMetric        = "probe_body_errors"
	probeFetchErrorsMetric       = "probe_fetch_errors"
	probeMetricsFailedMetric     = "probe_metrics_failed"
	probeMetricsSuccessfulMetric = "probe_metrics_successful"
	probeSuccessMetric           = "probe_success"
	probeTimestampErrorsMetric   = "probe_timestamp_errors"
)

var reservedProbeMetrics = map[string]struct{}{
	probeBodyErrorsMetric:        {},
	probeFetchErrorsMetric:       {},
	probeMetricsFailedMetric:     {},
	probeMetricsSuccessfulMetric: {},
	probeSuccessMetric:           {},
	probeTimestampErrorsMetric:   {},
}

// Run starts the exporter using the process command-line flags.
func Run(linkedVersion string) error {
	flag.Parse()
	if *showVersion {
		fmt.Println(resolveVersion(linkedVersion, debug.ReadBuildInfo))
		return nil
	}

	initLogger(*loglevel, *logFormat)

	if err := validateHTTPLimits(*maxResponseBodySize, *targetTimeout, *readHeaderTimeout); err != nil {
		return err
	}

	httpClient := newHTTPClient(*enableFileTransport, *enableUnixSocketTransport, *targetTimeout)
	handler, err := newHandler(*config, *expandEnv, *exposeMetadata, httpClient, *maxResponseBodySize, linkedVersion)
	if err != nil {
		return err
	}

	slog.Info("listening", "addr", *addr)
	server := newHTTPServer(*addr, handler, *readHeaderTimeout)
	return server.ListenAndServe()
}

func resolveVersion(linkedVersion string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if linkedVersion != "" {
		return linkedVersion
	}
	if buildInfo, ok := readBuildInfo(); ok && buildInfo.Main.Version != "" {
		return buildInfo.Main.Version
	}
	return "unknown"
}

func validateHTTPLimits(maxResponseBodySize int64, targetTimeout, readHeaderTimeout time.Duration) error {
	if maxResponseBodySize <= 0 {
		return fmt.Errorf("max-response-body-size must be positive")
	}
	if targetTimeout <= 0 {
		return fmt.Errorf("target-timeout must be positive")
	}
	if readHeaderTimeout <= 0 {
		return fmt.Errorf("read-header-timeout must be positive")
	}
	return nil
}

func newHTTPServer(addr string, handler http.Handler, readHeaderTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

func newHandler(config string, expandEnv, exposeMetadata bool, httpClient *http.Client, maxResponseBodySize int64, linkedVersion string) (http.Handler, error) {
	cfg, err := loadConfig(config, expandEnv)
	if err != nil {
		return nil, err
	}

	metrics.ExposeMetadata(exposeMetadata)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", handleMetrics(newBuildInfoMetricSet(resolveVersion(linkedVersion, debug.ReadBuildInfo))))
	mux.HandleFunc("GET /probe", handleProbe(cfg, httpClient, maxResponseBodySize))
	return mux, nil
}

func initLogger(loglevel, logFormat string) {
	slog.SetDefault(slog.New(newLogHandler(os.Stderr, loglevel, logFormat)))
}

func newLogHandler(w io.Writer, loglevel, logFormat string) slog.Handler {
	slogLevel := slog.LevelInfo
	switch strings.ToLower(loglevel) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	}

	options := &slog.HandlerOptions{
		AddSource: true,
		Level:     slogLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				source := a.Value.Any().(*slog.Source)
				source.File = filepath.Base(source.File)
			}
			return a
		},
	}
	if strings.EqualFold(logFormat, "json") {
		return slog.NewJSONHandler(w, options)
	}
	return slog.NewTextHandler(w, options)
}
