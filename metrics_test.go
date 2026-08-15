package main

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

func TestEscapeLabelValue(t *testing.T) {
	tests := map[string]string{
		"plain":                "plain",
		`quote"`:               `quote\"`,
		"line\nbreak":          `line\nbreak`,
		`backslash\`:           `backslash\\`,
		"quote\"\nbackslash\\": `quote\"\nbackslash\\`,
	}
	for value, want := range tests {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			assert(t, want, escapeLabelValue(value))
		})
	}
}

func TestAsCounterValueRejectsNegativeInt(t *testing.T) {
	if _, err := asCounterValue(-1); err == nil {
		t.Fatal("expected negative counter value error")
	}
}

func TestAsCounterValueRejectsInvalidFloat64(t *testing.T) {
	for name, value := range map[string]float64{
		"negative":          -1,
		"fractional":        1.5,
		"not a number":      math.NaN(),
		"positive infinity": math.Inf(1),
		"out of range":      float64(math.MaxUint64),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := asCounterValue(value); err == nil {
				t.Fatalf("expected error for %v", value)
			}
		})
	}
}

func TestCompiledNameAndLabelDoNotFallbackAfterEvaluationErrors(t *testing.T) {
	tests := map[string]Metric{
		"name": {
			Name:      `error("failed name")`,
			ValueType: valueTypeGauge,
			Value:     "1",
		},
		"label": {
			Name:      "probe_value",
			Labels:    map[string]Query{"kind": `error("failed label")`},
			ValueType: valueTypeGauge,
			Value:     "1",
		},
	}

	for name, metric := range tests {
		t.Run(name, func(t *testing.T) {
			err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, metric))
			if err == nil {
				t.Fatal("expected jq evaluation error")
			}
			if !strings.Contains(err.Error(), "failed "+name) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMakeMetricsEpochTimestampValueTypes(t *testing.T) {
	metricSet := newProbeMetricSet()
	for _, metric := range []Metric{
		{Name: "probe_counter", ValueType: valueTypeCounter, Value: "3", EpochTimestamp: ".timestamp"},
		{Name: "probe_gauge", ValueType: valueTypeGauge, Value: "1.5", EpochTimestamp: ".timestamp"},
		{Name: "probe_untyped", ValueType: valueTypeUntyped, Value: "-2", EpochTimestamp: ".timestamp"},
	} {
		if err := makeMetrics(t.Context(), metricSet, map[string]any{"timestamp": float64(1712345678901)}, mustCompileMetric(t, metric)); err != nil {
			t.Fatal(err)
		}
	}

	metrics.ExposeMetadata(false)
	var output strings.Builder
	metricSet.WritePrometheus(&output)
	assert(t, trim(`
probe_counter{} 3 1712345678901
probe_gauge{} 1.5 1712345678901
probe_untyped{} -2 1712345678901
`), output.String())
}

func TestMakeMetricsEpochTimestampFallback(t *testing.T) {
	tests := map[string]struct {
		query Query
		value any
	}{
		"query evaluation error": {query: `error("failed")`, value: map[string]any{"timestamp": 1}},
		"fractional value": {
			query: ".timestamp",
			value: map[string]any{"timestamp": 1.5},
		},
		"out of range": {
			query: ".timestamp",
			value: map[string]any{"timestamp": "9223372036854775808"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			err := makeMetrics(t.Context(), metricSet, test.value, mustCompileMetric(t, Metric{
				Name:           "probe_value",
				ValueType:      valueTypeGauge,
				Value:          "1",
				EpochTimestamp: test.query,
			}))
			if err != nil {
				t.Fatal(err)
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WriteProbeResult(&output, false)
			assert(t, probeStatus(0, 0, 0, 1, 0, 1)+"probe_value{} 1\n", output.String())
		})
	}
}

func TestMakeMetricsEpochTimestampOptional(t *testing.T) {
	tests := map[string]struct {
		query Query
		value any
	}{
		"null":      {query: ".timestamp", value: map[string]any{"timestamp": nil}},
		"no result": {query: ".timestamp // empty", value: map[string]any{}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			err := makeMetrics(t.Context(), metricSet, test.value, mustCompileMetric(t, Metric{
				Name:           "probe_value",
				ValueType:      valueTypeGauge,
				Value:          "1",
				EpochTimestamp: test.query,
			}))
			if err != nil {
				t.Fatal(err)
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WriteProbeResult(&output, false)
			assert(t, probeStatus(0, 0, 0, 1, 1, 0)+"probe_value{} 1\n", output.String())
		})
	}
}

func TestMakeMetricsUntyped(t *testing.T) {
	metricSet := newProbeMetricSet()
	metric := Metric{
		Name:      "probe_value",
		Labels:    map[string]Query{"id": ".id"},
		ValueType: valueTypeUntyped,
		Value:     ".value",
	}
	for _, value := range []any{
		map[string]any{"id": "b", "value": -2},
		map[string]any{"id": "a", "value": 1.5},
	} {
		if err := makeMetrics(t.Context(), metricSet, value, mustCompileMetric(t, metric)); err != nil {
			t.Fatal(err)
		}
	}

	metrics.ExposeMetadata(false)
	var withoutMetadata strings.Builder
	metricSet.WritePrometheus(&withoutMetadata)
	assert(t, trim(`
probe_value{id="a"} 1.5
probe_value{id="b"} -2
`), withoutMetadata.String())

	metrics.ExposeMetadata(true)
	t.Cleanup(func() { metrics.ExposeMetadata(false) })
	var withMetadata strings.Builder
	metricSet.WritePrometheus(&withMetadata)
	assert(t, trim(`
# HELP probe_value
# TYPE probe_value untyped
probe_value{id="a"} 1.5
probe_value{id="b"} -2
`), withMetadata.String())
}

func TestMakeMetricsRejectsConflictingValueTypes(t *testing.T) {
	metricSet := newProbeMetricSet()
	for _, valueType := range []string{valueTypeGauge, valueTypeUntyped} {
		err := makeMetrics(t.Context(), metricSet, nil, mustCompileMetric(t, Metric{
			Name:      "probe_value",
			ValueType: valueType,
			Value:     "1",
		}))
		if valueType == valueTypeGauge && err != nil {
			t.Fatal(err)
		}
		if valueType == valueTypeUntyped {
			if err == nil {
				t.Fatal("expected conflicting valueType error")
			}
			if !strings.Contains(err.Error(), "conflicting valueTypes") {
				t.Fatalf("unexpected error: %v", err)
			}
		}
	}
}

func TestMakeMetricsRejectsUnknownValueType(t *testing.T) {
	err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, Metric{
		Name:      "probe_value",
		ValueType: "unknown",
		Value:     "1",
	}))
	if err == nil {
		t.Fatal("expected unsupported valueType error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMakeMetricsRejectsInvalidNames(t *testing.T) {
	tests := map[string]Metric{
		"metric name": {
			Name:      `"invalid-name"`,
			ValueType: "gauge",
			Value:     "1",
		},
		"label name": {
			Name:      `"valid_metric"`,
			Labels:    map[string]Query{"invalid-label": `"value"`},
			ValueType: "gauge",
			Value:     "1",
		},
	}
	for name, metric := range tests {
		t.Run(name, func(t *testing.T) {
			err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, metric))
			if err == nil {
				t.Fatal("expected invalid metric error")
			}
			if !strings.Contains(err.Error(), "invalid metric") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
