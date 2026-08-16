package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/VictoriaMetrics/metrics"
)

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

func newBuildInfoMetricSet(version string) *metrics.Set {
	metricSet := metrics.NewSet()
	metricSet.NewGauge(buildInfoMetric+`{version="`+escapeLabelValue(version)+`"}`, func() float64 { return 1 })
	return metricSet
}

func handleMetrics(buildInfoMetrics *metrics.Set) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		buildInfoMetrics.WritePrometheus(w)
		metrics.WriteProcessMetrics(w)
	}
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
