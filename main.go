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
	"text/template"

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

	cfg, err := loadConfig(*config, *expandEnv)
	if err != nil {
		log.Fatal(err)
	}

	metrics.ExposeMetadata(*exposeMetadata)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("GET /probe", handleProbe(cfg))

	slog.Info("listening", "addr", *addr)
	http.ListenAndServe(*addr, mux)
}

func jq(ctx context.Context, query Query, value any, fallback bool) (any, error) {
	q, err := gojq.Parse(query)
	if err != nil {
		return nil, err
	}
	iter := q.RunWithContext(ctx, value)
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
			if fallback {
				// fallback to use query as result
				return query, nil
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

func makeLabelKV(ctx context.Context, labels map[string]Query, value any) (string, error) {
	var labelKV []string
	for labelName, labelQuery := range labels {
		_labelValue, err := jq(ctx, labelQuery, value, true)
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

func doHTTP(ctx context.Context, method string, target string, headers map[string]string, body io.Reader, validStatusCodes []int) (any, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
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

func makeBodyFromTemplate(data any, tmplString string) (io.Reader, error) {
	if tmplString == "" {
		return nil, nil
	}
	tmpl, err := template.New("").Parse(tmplString)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, data); err != nil {
		return nil, err
	}
	return buf, nil
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

		body, err := makeBodyFromTemplate(q, mod.Body.Content)
		if err != nil {
			slog.Error(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		var bodyJSON any
		bodyJSON, err = doHTTP(ctx, method, target, mod.Headers, body, mod.ValidStatusCodes)
		if err != nil {
			slog.Error(err.Error())
			http.Error(w, "Failed to fetch JSON response. TARGET: "+target+", ERROR: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		metricSet := newProbeMetricSet()
		for _, m := range mod.Metrics {
			var value any
			if m.Query == "" {
				value = bodyJSON
			} else {
				value, err = jq(ctx, m.Query, bodyJSON, false)
				if err != nil {
					slog.Error(err.Error())
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					return
				}
			}
			values := asSlice(value)

			for _, value := range values {
				if err := makeMetrics(ctx, metricSet, value, m); err != nil {
					slog.Error(err.Error())
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					return
				}
			}
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

func makeEpochTimestamp(ctx context.Context, query Query, value any) (*int64, error) {
	if query == "" {
		return nil, nil
	}
	result, err := jq(ctx, query, value, false)
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
	nameResult, err := jq(ctx, m.Name, value, true)
	if err != nil {
		return err
	}
	name.WriteString(fmt.Sprint(nameResult))
	name.WriteString("{")
	labelKV, err := makeLabelKV(ctx, m.Labels, value)
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

	v, err := jq(ctx, m.Value, value, false)
	if err != nil {
		return err
	}
	epochTimestamp, err := makeEpochTimestamp(ctx, m.EpochTimestamp, value)
	if err != nil {
		slog.Error("failed to extract epoch timestamp for metric", "metric", metricName, "query", m.EpochTimestamp, "error", err)
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
	return &cfg, nil
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
}

type Query = string

type Body struct {
	Content string `yaml:"content"`
}

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (fn RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
