package main

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

func TestHTTPResourceLimits(t *testing.T) {
	assert(t, defaultMaxResponseBodySize, *maxResponseBodySize)
	assert(t, defaultTargetTimeout, *targetTimeout)
	assert(t, defaultReadHeaderTimeout, *readHeaderTimeout)

	clientTimeout := 250 * time.Millisecond
	client := newHTTPClient(false, false, clientTimeout)
	assert(t, clientTimeout, client.Timeout)

	serverTimeout := 750 * time.Millisecond
	handler := http.NotFoundHandler()
	server := newHTTPServer("127.0.0.1:0", handler, serverTimeout)
	assert(t, "127.0.0.1:0", server.Addr)
	if server.Handler == nil {
		t.Fatal("HTTP server handler is nil")
	}
	assert(t, serverTimeout, server.ReadHeaderTimeout)
}

func TestValidateHTTPLimits(t *testing.T) {
	if err := validateHTTPLimits(defaultMaxResponseBodySize, defaultTargetTimeout, defaultReadHeaderTimeout); err != nil {
		t.Fatalf("valid defaults were rejected: %v", err)
	}
	tests := map[string]struct {
		maxResponseBodySize int64
		targetTimeout       time.Duration
		readHeaderTimeout   time.Duration
		want                string
	}{
		"zero response body size":      {0, defaultTargetTimeout, defaultReadHeaderTimeout, "max-response-body-size"},
		"negative response body size":  {-1, defaultTargetTimeout, defaultReadHeaderTimeout, "max-response-body-size"},
		"zero target timeout":          {defaultMaxResponseBodySize, 0, defaultReadHeaderTimeout, "target-timeout"},
		"negative target timeout":      {defaultMaxResponseBodySize, -1, defaultReadHeaderTimeout, "target-timeout"},
		"zero read header timeout":     {defaultMaxResponseBodySize, defaultTargetTimeout, 0, "read-header-timeout"},
		"negative read header timeout": {defaultMaxResponseBodySize, defaultTargetTimeout, -1, "read-header-timeout"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateHTTPLimits(test.maxResponseBodySize, test.targetTimeout, test.readHeaderTimeout)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s error, got %v", test.want, err)
			}
		})
	}
}

func TestProductionFlagsExpandEnvAndDefaultMetadata(t *testing.T) {
	if !*exposeMetadata {
		t.Fatal("production expose-metadata default must be true")
	}
	t.Setenv("PROMETHEUS_JQ_EXPORTER_METRIC_NAME", "expanded_counter")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := `modules:
  test:
    metrics:
      - name: ${PROMETHEUS_JQ_EXPORTER_METRIC_NAME}
        valueType: counter
        value: .value
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	originalConfig, originalExpandEnv := *config, *expandEnv
	t.Cleanup(func() {
		_ = flag.Set("config", originalConfig)
		_ = flag.Set("expand-env", strconv.FormatBool(originalExpandEnv))
	})
	if err := flag.Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("expand-env", "true"); err != nil {
		t.Fatal(err)
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":3}`)
	}))
	t.Cleanup(target.Close)
	t.Cleanup(func() { metrics.ExposeMetadata(false) })

	handler := must[http.Handler](t)(newHandler(*config, *expandEnv, *exposeMetadata, http.DefaultClient, defaultMaxResponseBodySize))
	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handler)
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, trim(`
# HELP expanded_counter
# TYPE expanded_counter counter
expanded_counter{} 3
# HELP probe_body_errors
# TYPE probe_body_errors gauge
probe_body_errors 0
# HELP probe_fetch_errors
# TYPE probe_fetch_errors gauge
probe_fetch_errors 0
# HELP probe_metrics_failed
# TYPE probe_metrics_failed gauge
probe_metrics_failed 0
# HELP probe_metrics_successful
# TYPE probe_metrics_successful gauge
probe_metrics_successful 1
# HELP probe_success
# TYPE probe_success gauge
probe_success 1
# HELP probe_timestamp_errors
# TYPE probe_timestamp_errors gauge
probe_timestamp_errors 0
`), body)
}
