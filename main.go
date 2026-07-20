package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/goccy/go-yaml"
	"github.com/itchyny/gojq"
)

var (
	addr                      = flag.String("addr", ":9999", "listen addr")
	config                    = flag.String("config", "config.yaml", "config file path")
	expandEnv                 = flag.Bool("expand-env", false, "expand environment variable in config file")
	loglevel                  = flag.String("log-level", "info", "log level")
	exposeMetadata            = flag.Bool("expose-metadata", true, "expose metric metadata")
	enableFileTransport       = flag.Bool("enable-file-transport", false, "enable file transport")
	enableUnixSocketTransport = flag.Bool("enable-unix-socket-transport", false, "enable unix socket transport")
	maxResponseBodySize       = flag.Int64("max-response-body-size", defaultMaxResponseBodySize, "maximum target response body size in bytes")
	targetTimeout             = flag.Duration("target-timeout", defaultTargetTimeout, "target request timeout")
	readHeaderTimeout         = flag.Duration("read-header-timeout", defaultReadHeaderTimeout, "HTTP server request header read timeout")
)

const (
	defaultMaxResponseBodySize int64 = 10 << 20
	defaultTargetTimeout             = 30 * time.Second
	defaultReadHeaderTimeout         = 5 * time.Second

	valueTypeCounter             = "counter"
	valueTypeGauge               = "gauge"
	valueTypeUntyped             = "untyped"
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

func main() {
	flag.Parse()

	initLogger(*loglevel)

	if err := validateHTTPLimits(*maxResponseBodySize, *targetTimeout, *readHeaderTimeout); err != nil {
		log.Fatal(err)
	}

	httpClient := newHTTPClient(*enableFileTransport, *enableUnixSocketTransport, *targetTimeout)
	handler, err := newHandler(*config, *expandEnv, *exposeMetadata, httpClient, *maxResponseBodySize)
	if err != nil {
		log.Fatal(err)
	}

	slog.Info("listening", "addr", *addr)
	server := newHTTPServer(*addr, handler, *readHeaderTimeout)
	log.Fatal(server.ListenAndServe())
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

func newHandler(config string, expandEnv, exposeMetadata bool, httpClient *http.Client, maxResponseBodySize int64) (http.Handler, error) {
	cfg, err := loadConfig(config, expandEnv)
	if err != nil {
		return nil, err
	}

	metrics.ExposeMetadata(exposeMetadata)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("GET /probe", handleProbe(cfg, httpClient, maxResponseBodySize))
	return mux, nil
}

func jqAll(ctx context.Context, query compiledQuery, value any) ([]any, error) {
	if query.code == nil {
		return []any{query.source}, nil
	}
	iter := query.code.RunWithContext(ctx, value)
	var results []any
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			if err, ok := err.(*gojq.HaltError); ok && err.Value() == nil {
				break
			}
			return nil, err
		}
		results = append(results, v)
	}
	return results, nil
}

func jqOne(ctx context.Context, query compiledQuery, value any) (any, error) {
	if query.code == nil {
		return query.source, nil
	}
	results, err := jqAll(ctx, query, value)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("query %q produced no value", query.source)
	}
	if len(results) > 1 {
		return nil, fmt.Errorf("query %q produced %d values, expected one", query.source, len(results))
	}
	return results[0], nil
}

func newHTTPClient(enableFileTransport, enableUnixSocketTransport bool, timeout time.Duration) *http.Client {
	defaultTransport := http.DefaultTransport.(*http.Transport).Clone()

	if enableFileTransport {
		fileTransport := http.NewFileTransport(http.Dir("."))
		defaultTransport.RegisterProtocol("file", RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "." {
				r.URL.Path = path.Join(r.URL.Host, r.URL.Path)
				r.URL.Host = "."
			}
			return fileTransport.RoundTrip(r)
		}))
	}

	var transport http.RoundTripper
	if enableUnixSocketTransport {
		transport = transportWithUnixSupport(defaultTransport)
	} else {
		transport = defaultTransport
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func transportWithUnixSupport(transport *http.Transport) http.RoundTripper {
	unixTransport := transport.Clone()
	unixTransport.Proxy = nil
	dialer := &net.Dialer{}
	unixTransport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		encodedPath, ok := strings.CutSuffix(host, ".unix")
		if !ok {
			return nil, fmt.Errorf("invalid unix socket address: %s", address)
		}
		socketPath, err := base64.RawURLEncoding.DecodeString(encodedPath)
		if err != nil {
			return nil, fmt.Errorf("invalid unix socket address %s: %w", address, err)
		}
		return dialer.DialContext(ctx, "unix", string(socketPath))
	}

	return &unixSupportTransport{
		transport:     transport,
		unixTransport: unixTransport,
	}
}

type unixSupportTransport struct {
	transport     *http.Transport
	unixTransport *http.Transport
}

func (t *unixSupportTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "unix" {
		return t.transport.RoundTrip(r)
	}
	if r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, "/") {
		return nil, fmt.Errorf("invalid unix socket URL: expected unix:///absolute/path.sock[/resource]")
	}

	parts := strings.Split(r.URL.Path, "/")
	for i, part := range parts {
		if !strings.HasSuffix(part, ".sock") {
			continue
		}

		originalRequest := r
		r = r.Clone(r.Context())
		socketPath := strings.Join(parts[:i+1], "/")
		resourcePath := "/"
		if i+1 < len(parts) {
			resourcePath += strings.Join(parts[i+1:], "/")
		}
		if r.Host == "" {
			r.Host = r.Header.Get("Host")
			if r.Host == "" {
				r.Host = "localhost"
			}
		}
		r.URL.Scheme = "http"
		r.URL.Host = base64.RawURLEncoding.EncodeToString([]byte(socketPath)) + ".unix"
		r.URL.Path = resourcePath
		r.URL.RawPath = ""
		resp, err := t.unixTransport.RoundTrip(r)
		if resp != nil {
			resp.Request = originalRequest
		}
		return resp, err
	}
	return nil, fmt.Errorf("invalid unix socket URL: path must contain a segment ending in .sock")
}

func (t *unixSupportTransport) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
	t.unixTransport.CloseIdleConnections()
}

func initLogger(loglevel string) {
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

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slogLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				source := a.Value.Any().(*slog.Source)
				source.File = filepath.Base(source.File)
			}
			return a
		}},
	)))
}

func makeLabelKV(ctx context.Context, labels map[string]compiledQuery, value any) (string, error) {
	var labelKV []string
	for labelName, labelQuery := range labels {
		_labelValue, err := jqOne(ctx, labelQuery, value)
		if err != nil {
			return "", err
		}
		labelValue := asLabelValue(_labelValue)
		labelKV = append(labelKV, fmt.Sprintf(`%s="%s"`, labelName, escapeLabelValue(labelValue)))
	}
	slices.Sort(labelKV)
	return strings.Join(labelKV, ","), nil
}

var labelValueEscaper = strings.NewReplacer(
	`\`, `\\`,
	"\n", `\n`,
	`"`, `\"`,
)

func escapeLabelValue(value string) string {
	return labelValueEscaper.Replace(value)
}

func isIntegralInRange(v, min, max float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v && v >= min && v < max
}

func asCounterValue(value any) (uint64, error) {
	var u64Value uint64
	switch v := value.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("counter value %d must not be negative", v)
		}
		u64Value = uint64(v)
	case float64:
		if !isIntegralInRange(v, 0, float64(math.MaxUint64)) {
			return 0, fmt.Errorf("counter value %v is not a uint64", v)
		}
		u64Value = uint64(v)
	default:
		var err error
		u64Value, err = strconv.ParseUint(fmt.Sprint(v), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	return u64Value, nil
}

func asGaugeValue(value any) (float64, error) {
	var floatValue float64
	switch v := value.(type) {
	case int:
		floatValue = float64(v)
	case float64:
		floatValue = v
	default:
		var err error
		floatValue, err = strconv.ParseFloat(fmt.Sprint(v), 10)
		if err != nil {
			return 0, err
		}
	}
	return floatValue, nil
}

func asSlice(value any) []any {
	if value == nil {
		return []any{nil}
	}
	var values []any
	switch reflect.TypeOf(value).Kind() {
	case reflect.Slice:
		values = value.([]any)
	default:
		values = []any{value}
	}
	return values
}

func asLabelValue(value any) string {
	var labelValue string
	switch lv := value.(type) {
	case bool:
		labelValue = strconv.FormatBool(lv)
	default:
		labelValue = fmt.Sprint(lv)
	}
	return labelValue
}

func doHTTP(ctx context.Context, httpClient *http.Client, method string, target string, headers map[string]string, body io.Reader, bodyContentType string, validStatusCodes []int, maxResponseBodySize int64) (any, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if bodyContentType != "" {
		req.Header.Set("Content-Type", bodyContentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if len(validStatusCodes) == 0 {
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("unexpected HTTP status: %s", resp.Status)
		}
	} else if !slices.Contains(validStatusCodes, resp.StatusCode) {
		return nil, fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	b, err := readResponseBody(resp.Body, maxResponseBodySize)
	if err != nil {
		return nil, err
	}

	var respBodyJSON any
	if err := json.Unmarshal(b, &respBodyJSON); err != nil {
		return nil, fmt.Errorf("%s: %w", string(b), err)
	}
	return respBodyJSON, nil
}

func readResponseBody(r io.Reader, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("maximum response body size must be positive")
	}
	b, err := io.ReadAll(http.MaxBytesReader(nil, io.NopCloser(r), maxSize))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, fmt.Errorf("target response body exceeds %d bytes", maxSize)
		}
		return nil, err
	}
	return b, nil
}

func makeBody(ctx context.Context, params map[string][]string, body Body) (io.Reader, string, error) {
	if body.query == nil {
		return nil, "", nil
	}
	input := make(map[string]any, len(params))
	for key, values := range params {
		switch key {
		case "module", "target", "method", "debug":
			continue
		}
		items := make([]any, len(values))
		for i, value := range values {
			items[i] = value
		}
		input[key] = items
	}
	value, err := jqOne(ctx, *body.query, input)
	if err != nil {
		return nil, "", err
	}
	switch body.format {
	case bodyFormatJSON:
		content, err := json.Marshal(value)
		if err != nil {
			return nil, "", err
		}
		return bytes.NewReader(content), "application/json", nil
	case bodyFormatText:
		content, ok := value.(string)
		if !ok {
			return nil, "", fmt.Errorf("body.text must produce a string, got %T", value)
		}
		return strings.NewReader(content), "text/plain; charset=utf-8", nil
	default:
		return nil, "", fmt.Errorf("unsupported body format %q", body.format)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics.WriteProcessMetrics(w)
}

func handleProbe(cfg *Config, httpClient *http.Client, maxResponseBodySize int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ctx := r.Context()
		module := q.Get("module")
		if module == "" {
			slog.Warn("no module found in query")
			http.Error(w, "Module parameter is missing", http.StatusBadRequest)
			return
		}
		mod, ok := cfg.Modules[module]
		if !ok {
			slog.Warn("no module found in config", "module", module)
			http.Error(w, fmt.Sprintf("Unknown module %q", module), http.StatusBadRequest)
			return
		}

		target := q.Get("target")
		if target == "" {
			slog.Warn("no target found in query")
			http.Error(w, "Target parameter is missing", http.StatusBadRequest)
			return
		}

		method := q.Get("method")
		if method == "" {
			method = http.MethodGet
		}

		slog.Debug("start probe", "module", module, "method", method, "target", target)

		debug := q.Get("debug") == "true"
		metricSet := newProbeMetricSet()
		body, bodyContentType, err := makeBody(ctx, q, mod.Body)
		if err != nil {
			slog.Error(err.Error())
			metricSet.recordBodyError(err)
			metricSet.WriteProbeResult(w, debug)
			return
		}
		var bodyJSON any
		bodyJSON, err = doHTTP(ctx, httpClient, method, target, mod.Headers, body, bodyContentType, mod.ValidStatusCodes, maxResponseBodySize)
		if err != nil {
			slog.Error(err.Error())
			metricSet.recordFetchError(err)
			metricSet.WriteProbeResult(w, debug)
			return
		}

		for metricIndex, m := range mod.Metrics {
			var values []any
			if m.query == nil {
				values = asSlice(bodyJSON)
			} else {
				outputs, err := jqAll(ctx, *m.query, bodyJSON)
				if err != nil {
					metricSet.recordMetricError(err)
					slog.Error("failed to query metric values", "metric_index", metricIndex, "metric", m.Name, "query", m.Query, "error", err)
					continue
				}
				for _, output := range outputs {
					values = append(values, asSlice(output)...)
				}
			}

			for valueIndex, value := range values {
				if err := makeMetrics(ctx, metricSet, value, m); err != nil {
					metricSet.recordMetricError(err)
					slog.Error("failed to make metric", "metric_index", metricIndex, "metric", m.Name, "value_index", valueIndex, "error", err)
					continue
				}
			}
		}
		metricSet.WriteProbeResult(w, q.Get("debug") == "true")
	}
}

type probeMetric struct {
	family         string
	name           string
	valueType      string
	counterValue   uint64
	floatValue     float64
	epochTimestamp *int64
}

type probeMetricSet struct {
	metrics           map[string]probeMetric
	familyTypes       map[string]string
	success           bool
	bodyErrors        int
	fetchErrors       int
	metricsFailed     int
	metricsSuccessful int
	timestampErrors   int
	errors            []error
}

func newProbeMetricSet() *probeMetricSet {
	return &probeMetricSet{
		metrics:     make(map[string]probeMetric),
		familyTypes: make(map[string]string),
		success:     true,
	}
}

func (s *probeMetricSet) recordError(err error) {
	s.success = false
	s.errors = append(s.errors, err)
}

func (s *probeMetricSet) recordBodyError(err error) {
	s.bodyErrors++
	s.recordError(err)
}

func (s *probeMetricSet) recordFetchError(err error) {
	s.fetchErrors++
	s.recordError(err)
}

func (s *probeMetricSet) recordMetricError(err error) {
	s.metricsFailed++
	s.recordError(err)
}

func (s *probeMetricSet) recordTimestampError(err error) {
	s.timestampErrors++
	s.recordError(err)
}

func (s *probeMetricSet) addGauge(name string, value float64) {
	s.metrics[name] = probeMetric{
		family:     name,
		name:       name,
		valueType:  valueTypeGauge,
		floatValue: value,
	}
}

func (s *probeMetricSet) WriteProbeResult(w io.Writer, debug bool) {
	if debug {
		for _, err := range s.errors {
			fmt.Fprintf(w, "# probe_error %q\n", err.Error())
		}
	}
	s.addGauge(probeBodyErrorsMetric, float64(s.bodyErrors))
	s.addGauge(probeFetchErrorsMetric, float64(s.fetchErrors))
	s.addGauge(probeMetricsFailedMetric, float64(s.metricsFailed))
	s.addGauge(probeMetricsSuccessfulMetric, float64(s.metricsSuccessful))
	success := float64(0)
	if s.success {
		success = 1
	}
	s.addGauge(probeSuccessMetric, success)
	s.addGauge(probeTimestampErrorsMetric, float64(s.timestampErrors))
	s.WritePrometheus(w)
}

func (s *probeMetricSet) registerFamily(family, valueType string) error {
	if existingType, ok := s.familyTypes[family]; ok && existingType != valueType {
		return fmt.Errorf("metric family %s has conflicting valueTypes %s and %s", family, existingType, valueType)
	}
	s.familyTypes[family] = valueType
	return nil
}

func (s *probeMetricSet) WritePrometheus(w io.Writer) {
	probeMetrics := make([]probeMetric, 0, len(s.metrics))
	for _, m := range s.metrics {
		probeMetrics = append(probeMetrics, m)
	}
	slices.SortFunc(probeMetrics, func(a, b probeMetric) int {
		return cmp.Or(cmp.Compare(a.family, b.family), cmp.Compare(a.name, b.name))
	})
	previousFamily := ""
	for _, m := range probeMetrics {
		if m.family != previousFamily {
			metrics.WriteMetadataIfNeeded(w, m.family, m.valueType)
			previousFamily = m.family
		}
		fmt.Fprint(w, m.name, " ")
		if m.valueType == valueTypeCounter {
			fmt.Fprint(w, m.counterValue)
		} else if isIntegralInRange(m.floatValue, math.MinInt64, float64(math.MaxInt64)) {
			fmt.Fprint(w, int64(m.floatValue))
		} else {
			fmt.Fprintf(w, "%g", m.floatValue)
		}
		if m.epochTimestamp != nil {
			fmt.Fprint(w, " ", *m.epochTimestamp)
		}
		fmt.Fprintln(w)
	}
}

func makeEpochTimestamp(ctx context.Context, query *compiledQuery, value any) (*int64, error) {
	if query == nil {
		return nil, nil
	}
	results, err := jqAll(ctx, *query, value)
	if err != nil {
		return nil, err
	}
	if len(results) > 1 {
		return nil, fmt.Errorf("timestamp query %q produced %d values, expected at most one", query.source, len(results))
	}
	if len(results) == 0 || results[0] == nil {
		return nil, nil
	}
	result := results[0]

	var timestamp int64
	switch v := result.(type) {
	case int:
		timestamp = int64(v)
	case int64:
		timestamp = v
	case float64:
		if !isIntegralInRange(v, math.MinInt64, float64(math.MaxInt64)) {
			return nil, fmt.Errorf("timestamp %v is not an int64", v)
		}
		timestamp = int64(v)
	case string:
		timestamp, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("timestamp %v is not an int64", result)
	}
	return &timestamp, nil
}

func makeMetrics(ctx context.Context, metricSet *probeMetricSet, value any, m Metric) error {
	nameResult, err := jqOne(ctx, m.name, value)
	if err != nil {
		return err
	}
	metricFamily := fmt.Sprint(nameResult)
	labelKV, err := makeLabelKV(ctx, m.labels, value)
	if err != nil {
		return err
	}
	metricName := metricFamily + "{" + labelKV + "}"
	if err := metrics.ValidateMetric(metricName); err != nil {
		return fmt.Errorf("invalid metric %q: %w", metricName, err)
	}
	if _, reserved := reservedProbeMetrics[metricFamily]; reserved {
		return fmt.Errorf("metric family %q is reserved", metricFamily)
	}

	v, err := jqOne(ctx, m.value, value)
	if err != nil {
		return err
	}
	epochTimestamp, err := makeEpochTimestamp(ctx, m.epochTimestamp, value)
	if err != nil {
		slog.Error("failed to extract epoch timestamp for metric", "metric", metricName, "query", m.epochTimestamp.source, "error", err)
		metricSet.recordTimestampError(err)
		epochTimestamp = nil
	}
	probeMetric := probeMetric{
		family:         metricFamily,
		name:           metricName,
		valueType:      m.ValueType,
		epochTimestamp: epochTimestamp,
	}

	switch m.ValueType {
	case valueTypeCounter:
		counterValue, err := asCounterValue(v)
		if err != nil {
			return err
		}
		probeMetric.counterValue = counterValue
	case valueTypeGauge, valueTypeUntyped:
		floatValue, err := asGaugeValue(v)
		if err != nil {
			return err
		}
		probeMetric.floatValue = floatValue
	default:
		return fmt.Errorf("valueType %s is not supported", m.ValueType)
	}
	if err := metricSet.registerFamily(metricFamily, m.ValueType); err != nil {
		return err
	}
	metricSet.metrics[metricName] = probeMetric
	metricSet.metricsSuccessful++
	return nil
}

func loadConfig(config string, expandEnv bool) (*Config, error) {
	b, err := os.ReadFile(config)
	if err != nil {
		return nil, err
	}

	if expandEnv {
		b = []byte(os.ExpandEnv(string(b)))
	}

	var cfg Config
	var unmarshal func(b []byte, dst any) error
	switch filepath.Ext(config) {
	case ".json":
		unmarshal = json.Unmarshal
	case ".yaml", ".yml":
		unmarshal = yaml.Unmarshal
	default:
		return nil, fmt.Errorf("unsupported file %s", config)
	}

	if err := unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if err := compileConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type compiledQuery struct {
	source Query
	code   *gojq.Code
}

type queryCompileResult struct {
	code *gojq.Code
	err  error
}

type queryCompiler map[Query]queryCompileResult

func (c queryCompiler) compile(source Query, literalFallback bool) (compiledQuery, error) {
	result, ok := c[source]
	if !ok {
		if query, err := gojq.Parse(source); err != nil {
			result.err = err
		} else {
			result.code, result.err = gojq.Compile(query)
		}
		c[source] = result
	}
	if result.err != nil {
		if literalFallback {
			return compiledQuery{source: source}, nil
		}
		return compiledQuery{}, result.err
	}
	return compiledQuery{source: source, code: result.code}, nil
}

var (
	metricNameRegexp   = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelLiteralRegexp = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

func compileName(compiler queryCompiler, source Query) (compiledQuery, error) {
	if metricNameRegexp.MatchString(source) {
		return compiledQuery{source: source}, nil
	}
	query, err := compiler.compile(source, false)
	if err != nil {
		return compiledQuery{}, fmt.Errorf("%q is neither a valid metric name nor a valid jq query: %w", source, err)
	}
	return query, nil
}

func compileLabelValue(compiler queryCompiler, source Query) (compiledQuery, error) {
	if labelLiteralRegexp.MatchString(source) {
		return compiledQuery{source: source}, nil
	}
	return compiler.compile(source, true)
}

func compileConfig(cfg *Config) error {
	compiler := make(queryCompiler)
	for moduleName, module := range cfg.Modules {
		compiledBody, err := compileBody(compiler, module.Body)
		if err != nil {
			return fmt.Errorf("module %q body: %w", moduleName, err)
		}
		module.Body = compiledBody
		for metricIndex, metric := range module.Metrics {
			compiledMetric, err := compileMetric(compiler, metric)
			if err != nil {
				return fmt.Errorf("module %q metric %d: %w", moduleName, metricIndex, err)
			}
			module.Metrics[metricIndex] = compiledMetric
		}
		cfg.Modules[moduleName] = module
	}
	return nil
}

func compileBody(compiler queryCompiler, body Body) (Body, error) {
	if body.JSON != nil && body.Text != nil {
		return Body{}, fmt.Errorf("json and text are mutually exclusive")
	}
	compiled := body
	var source *Query
	switch {
	case body.JSON != nil:
		compiled.format = bodyFormatJSON
		source = body.JSON
	case body.Text != nil:
		compiled.format = bodyFormatText
		source = body.Text
	default:
		return compiled, nil
	}
	if *source == "" {
		return Body{}, fmt.Errorf("%s query is empty", compiled.format)
	}
	query, err := compiler.compile(*source, false)
	if err != nil {
		return Body{}, fmt.Errorf("%s: %w", compiled.format, err)
	}
	compiled.query = &query
	return compiled, nil
}

func compileMetric(compiler queryCompiler, metric Metric) (Metric, error) {
	compiled := metric
	if compiled.ValueType == "" {
		compiled.ValueType = valueTypeUntyped
	}
	compiled.labels = make(map[string]compiledQuery, len(metric.Labels))
	var err error
	if metric.Query != "" {
		query, compileErr := compiler.compile(metric.Query, false)
		if compileErr != nil {
			return Metric{}, fmt.Errorf("query: %w", compileErr)
		}
		compiled.query = &query
	}
	compiled.name, err = compileName(compiler, metric.Name)
	if err != nil {
		return Metric{}, fmt.Errorf("name: %w", err)
	}
	if _, reserved := reservedProbeMetrics[compiled.name.source]; compiled.name.code == nil && reserved {
		return Metric{}, fmt.Errorf("name: metric family %q is reserved", compiled.name.source)
	}
	for labelName, labelQuery := range metric.Labels {
		compiledLabel, compileErr := compileLabelValue(compiler, labelQuery)
		if compileErr != nil {
			return Metric{}, fmt.Errorf("label %q: %w", labelName, compileErr)
		}
		compiled.labels[labelName] = compiledLabel
	}
	compiled.value, err = compiler.compile(metric.Value, false)
	if err != nil {
		return Metric{}, fmt.Errorf("value: %w", err)
	}
	if metric.EpochTimestamp != "" {
		epochTimestamp, compileErr := compiler.compile(metric.EpochTimestamp, false)
		if compileErr != nil {
			return Metric{}, fmt.Errorf("epochTimestamp: %w", compileErr)
		}
		compiled.epochTimestamp = &epochTimestamp
	}
	return compiled, nil
}

type Config struct {
	Modules map[string]Module `json:"modules" yaml:"modules"`
}

type Module struct {
	Metrics          []Metric          `json:"metrics" yaml:"metrics"`
	Body             Body              `json:"body" yaml:"body"`
	Headers          map[string]string `json:"headers" yaml:"headers"`
	ValidStatusCodes []int             `json:"valid_status_codes" yaml:"valid_status_codes"`
}

type Metric struct {
	Query          Query            `json:"query" yaml:"query"` // optional
	Name           Query            `json:"name" yaml:"name"`
	Labels         map[string]Query `json:"labels" yaml:"labels"`
	ValueType      string           `json:"valueType" yaml:"valueType"` // "counter", "gauge", "untyped" (default)
	Value          Query            `json:"value" yaml:"value"`
	EpochTimestamp Query            `json:"epochTimestamp" yaml:"epochTimestamp"` // optional, Unix milliseconds
	query          *compiledQuery
	name           compiledQuery
	labels         map[string]compiledQuery
	value          compiledQuery
	epochTimestamp *compiledQuery
}

type Query = string

type bodyFormat string

const (
	bodyFormatJSON bodyFormat = "json"
	bodyFormatText bodyFormat = "text"
)

type Body struct {
	JSON   *Query `json:"json,omitempty" yaml:"json,omitempty"`
	Text   *Query `json:"text,omitempty" yaml:"text,omitempty"`
	format bodyFormat
	query  *compiledQuery
}

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (fn RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
