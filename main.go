package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

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

	httpClient = sync.OnceValue(initHTTPClient)
)

const (
	valueTypeCounter = "counter"
	valueTypeGauge   = "gauge"
	valueTypeUntyped = "untyped"
)

func main() {
	flag.Parse()

	initLogger(*loglevel)

	handler, err := newHandler(*config, *expandEnv, *exposeMetadata)
	if err != nil {
		log.Fatal(err)
	}

	slog.Info("listening", "addr", *addr)
	http.ListenAndServe(*addr, handler)
}

func newHandler(config string, expandEnv, exposeMetadata bool) (http.Handler, error) {
	cfg, err := loadConfig(config, expandEnv)
	if err != nil {
		return nil, err
	}

	metrics.ExposeMetadata(exposeMetadata)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("GET /probe", handleProbe(cfg))
	return mux, nil
}

func jq(ctx context.Context, query compiledQuery, value any) (any, error) {
	if query.code == nil {
		return query.source, nil
	}
	iter := query.code.RunWithContext(ctx, value)
	var result any
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
		result = v
	}
	return result, nil
}

func initHTTPClient() *http.Client {
	defaultTransport := http.DefaultTransport.(*http.Transport)

	if *enableFileTransport {
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
	if *enableUnixSocketTransport {
		transport = transportWithUnixSupport(defaultTransport)
	} else {
		transport = defaultTransport
	}

	return &http.Client{
		Transport: transport,
	}
}

func transportWithUnixSupport(transport *http.Transport) http.RoundTripper {
	return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, ".sock") {
			parts := strings.Split(r.URL.Path, "/")
			for i, part := range parts {
				if i != len(parts)-1 && strings.HasSuffix(part, ".sock") {
					r.URL.Path = "/" + path.Join(parts[i+1:]...)
					if r.URL.Host == "" {
						if host := r.Header.Get("Host"); host != "" {
							r.URL.Host = host
						}
					}
					unixTransport := transport.Clone()
					socketPath := strings.Join(parts[0:i+1], "/")
					unixTransport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
						return net.Dial("unix", socketPath)
					}
					return unixTransport.RoundTrip(r)
				}
			}
		}
		return transport.RoundTrip(r)
	})
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
		_labelValue, err := jq(ctx, labelQuery, value)
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

func asCounterValue(value any) (uint64, error) {
	var u64Value uint64
	switch v := value.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("counter value %d must not be negative", v)
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

func doHTTP(ctx context.Context, method string, target string, headers map[string]string, body io.Reader, bodyContentType string, validStatusCodes []int) (any, error) {
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
	resp, err := httpClient().Do(req)
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

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var respBodyJSON any
	if err := json.Unmarshal(b, &respBodyJSON); err != nil {
		return nil, fmt.Errorf("%s: %w", string(b), err)
	}
	return respBodyJSON, nil
}

func makeBody(ctx context.Context, params map[string][]string, body Body) (io.Reader, string, error) {
	if body.query == nil {
		return nil, "", nil
	}
	input := make(map[string]any, len(params))
	for key, values := range params {
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

func jqOne(ctx context.Context, query compiledQuery, value any) (any, error) {
	iter := query.code.RunWithContext(ctx, value)
	var result any
	count := 0
	for {
		value, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := value.(error); ok {
			if err, ok := err.(*gojq.HaltError); ok && err.Value() == nil {
				break
			}
			return nil, err
		}
		count++
		if count > 1 {
			return nil, fmt.Errorf("body query produced multiple values")
		}
		result = value
	}
	if count == 0 {
		return nil, fmt.Errorf("body query produced no value")
	}
	return result, nil
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics.WriteProcessMetrics(w)
}

func handleProbe(cfg *Config) http.HandlerFunc {
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

		body, bodyContentType, err := makeBody(ctx, q, mod.Body)
		if err != nil {
			slog.Error(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		var bodyJSON any
		bodyJSON, err = doHTTP(ctx, method, target, mod.Headers, body, bodyContentType, mod.ValidStatusCodes)
		if err != nil {
			slog.Error(err.Error())
			http.Error(w, "Failed to fetch JSON response. TARGET: "+target+", ERROR: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		metricSet := newProbeMetricSet()
		successfulMetrics := 0
		metricErrors := 0
		for metricIndex, m := range mod.Metrics {
			var value any
			if m.query == nil {
				value = bodyJSON
			} else {
				value, err = jq(ctx, *m.query, bodyJSON)
				if err != nil {
					metricErrors++
					slog.Error("failed to query metric values", "metric_index", metricIndex, "metric", m.Name, "query", m.Query, "error", err)
					continue
				}
			}
			values := asSlice(value)

			for valueIndex, value := range values {
				if err := makeMetrics(ctx, metricSet, value, m); err != nil {
					metricErrors++
					slog.Error("failed to make metric", "metric_index", metricIndex, "metric", m.Name, "value_index", valueIndex, "error", err)
					continue
				}
				successfulMetrics++
			}
		}
		if metricErrors > 0 && successfulMetrics == 0 {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		metricSet.WritePrometheus(w)
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
	metrics     map[string]probeMetric
	familyTypes map[string]string
}

func newProbeMetricSet() *probeMetricSet {
	return &probeMetricSet{
		metrics:     make(map[string]probeMetric),
		familyTypes: make(map[string]string),
	}
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
	sort.Slice(probeMetrics, func(i, j int) bool {
		if probeMetrics[i].family != probeMetrics[j].family {
			return probeMetrics[i].family < probeMetrics[j].family
		}
		return probeMetrics[i].name < probeMetrics[j].name
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
		} else if float64(int64(m.floatValue)) == m.floatValue {
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
	result, err := jq(ctx, *query, value)
	if err != nil {
		return nil, err
	}

	var timestamp int64
	switch v := result.(type) {
	case int:
		timestamp = int64(v)
	case int64:
		timestamp = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < math.MinInt64 || v >= float64(math.MaxInt64) {
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
	var name strings.Builder
	nameResult, err := jq(ctx, m.name, value)
	if err != nil {
		return err
	}
	name.WriteString(fmt.Sprint(nameResult))
	name.WriteString("{")
	labelKV, err := makeLabelKV(ctx, m.labels, value)
	if err != nil {
		return err
	}
	name.WriteString(labelKV)
	name.WriteString("}")
	metricName := name.String()
	if err := metrics.ValidateMetric(metricName); err != nil {
		return fmt.Errorf("invalid metric %q: %w", metricName, err)
	}
	metricFamily := fmt.Sprint(nameResult)

	v, err := jq(ctx, m.value, value)
	if err != nil {
		return err
	}
	epochTimestamp, err := makeEpochTimestamp(ctx, m.epochTimestamp, value)
	if err != nil {
		slog.Error("failed to extract epoch timestamp for metric", "metric", metricName, "query", m.epochTimestamp.source, "error", err)
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
		if err := metricSet.registerFamily(metricFamily, m.ValueType); err != nil {
			return err
		}
		probeMetric.counterValue = counterValue
	case valueTypeGauge:
		gaugeValue, err := asGaugeValue(v)
		if err != nil {
			return err
		}
		if err := metricSet.registerFamily(metricFamily, m.ValueType); err != nil {
			return err
		}
		probeMetric.floatValue = gaugeValue
	case valueTypeUntyped:
		untypedValue, err := asGaugeValue(v)
		if err != nil {
			return err
		}
		if err := metricSet.registerFamily(metricFamily, m.ValueType); err != nil {
			return err
		}
		probeMetric.floatValue = untypedValue
	default:
		return fmt.Errorf("valueType %s is not supported", m.ValueType)
	}
	metricSet.metrics[metricName] = probeMetric
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
	for moduleName, module := range cfg.Modules {
		for i := range module.Metrics {
			if module.Metrics[i].ValueType == "" {
				module.Metrics[i].ValueType = valueTypeUntyped
			}
		}
		cfg.Modules[moduleName] = module
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
	code       *gojq.Code
	parseErr   error
	compileErr error
}

type queryCompiler map[Query]queryCompileResult

func (c queryCompiler) compile(source Query, literalFallback bool) (compiledQuery, error) {
	result, ok := c[source]
	if !ok {
		query, err := gojq.Parse(source)
		if err != nil {
			result.parseErr = err
		} else {
			result.code, result.compileErr = gojq.Compile(query)
		}
		c[source] = result
	}
	if result.parseErr != nil {
		return compiledQuery{}, result.parseErr
	}
	if result.compileErr != nil {
		if literalFallback {
			return compiledQuery{source: source}, nil
		}
		return compiledQuery{}, result.compileErr
	}
	return compiledQuery{source: source, code: result.code}, nil
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
	compiled.labels = make(map[string]compiledQuery, len(metric.Labels))
	var err error
	if metric.Query != "" {
		query, compileErr := compiler.compile(metric.Query, false)
		if compileErr != nil {
			return Metric{}, fmt.Errorf("query: %w", compileErr)
		}
		compiled.query = &query
	}
	compiled.name, err = compiler.compile(metric.Name, true)
	if err != nil {
		return Metric{}, fmt.Errorf("name: %w", err)
	}
	for labelName, labelQuery := range metric.Labels {
		compiledLabel, compileErr := compiler.compile(labelQuery, true)
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
