package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itchyny/gojq"
)

func TestLoadConfig(t *testing.T) {
	tests := map[string]struct {
		fileName   string
		content    string
		bodyFormat bodyFormat
	}{
		"yaml": {
			fileName: "config",
			content: `modules:
  test:
    valid_status_codes: [200, 404]
    body:
      json: '{value: .value[0]}'
    metrics:
      - name: test_metric
        value: 1
        epochTimestamp: .timestamp
`,
			bodyFormat: bodyFormatJSON,
		},
		"json": {
			fileName:   "config.conf",
			content:    `{"modules":{"test":{"valid_status_codes":[200,404],"body":{"text":"\"value=\\(.value[0])\""},"metrics":[{"name":"test_metric","value":"1","epochTimestamp":".timestamp"}]}}}`,
			bodyFormat: bodyFormatText,
		},
	}

	for format, test := range tests {
		t.Run(format, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), test.fileName)
			if err := os.WriteFile(configPath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			module := cfg.Modules["test"]
			assert(t, []int{http.StatusOK, http.StatusNotFound}, module.ValidStatusCodes)
			assert(t, valueTypeUntyped, module.Metrics[0].ValueType)
			assert(t, Query(".timestamp"), module.Metrics[0].EpochTimestamp)

			body := module.Body
			if body.query == nil {
				t.Fatal("body query was not compiled")
			}
			assert(t, test.bodyFormat, body.format)
		})
	}
}

func TestCompileConfigCompilesAndReusesQueries(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Body: Body{JSON: new(".value")},
			Metrics: []Metric{{
				Query:          ".value",
				Name:           ".value",
				Labels:         map[string]Query{"value": ".value"},
				ValueType:      valueTypeGauge,
				Value:          ".value",
				EpochTimestamp: ".value",
			}},
		},
	}}

	compiled := mustCompileConfig(t, cfg).Modules["test"].Metrics[0]
	wantCode := compiled.query.code
	for field, gotCode := range map[string]*gojq.Code{
		"body":           cfg.Modules["test"].Body.query.code,
		"name":           compiled.name.code,
		"label":          compiled.labels["value"].code,
		"value":          compiled.value.code,
		"epochTimestamp": compiled.epochTimestamp.code,
	} {
		if gotCode != wantCode {
			t.Errorf("%s did not reuse the compiled query", field)
		}
	}
}

func TestCompileConfigRejectsInvalidBodies(t *testing.T) {
	tests := map[string]struct {
		body Body
		part string
	}{
		"json and text": {
			body: Body{JSON: new("."), Text: new(".")},
			part: "mutually exclusive",
		},
		"empty json": {
			body: Body{JSON: new("")},
			part: "json query is empty",
		},
		"empty text": {
			body: Body{Text: new("")},
			part: "text query is empty",
		},
		"json parse error": {
			body: Body{JSON: new("(")},
			part: "json",
		},
		"text compile error": {
			body: Body{Text: new("unknown_function")},
			part: "text",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{"test": {Body: test.body}}})
			if err == nil {
				t.Fatal("expected body compilation error")
			}
			for _, part := range []string{`module "test"`, "body", test.part} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileConfigRejectsInvalidQueries(t *testing.T) {
	tests := map[string]struct {
		metric Metric
		field  string
	}{
		"query parse error": {
			metric: Metric{Query: "(", Name: "metric", Value: "1"},
			field:  "query",
		},
		"query compile error": {
			metric: Metric{Query: "unknown_function", Name: "metric", Value: "1"},
			field:  "query",
		},
		"name parse error": {
			metric: Metric{Name: "(", Value: "1"},
			field:  "name",
		},
		"name compile error": {
			metric: Metric{Name: "foo-bar", Value: "1"},
			field:  "name",
		},
		"value compile error": {
			metric: Metric{Name: "metric", Value: "unknown_function"},
			field:  "value",
		},
		"epoch timestamp parse error": {
			metric: Metric{Name: "metric", Value: "1", EpochTimestamp: "("},
			field:  "epochTimestamp",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{
				"test": {Metrics: []Metric{test.metric}},
			}})
			if err == nil {
				t.Fatal("expected query compilation error")
			}
			for _, part := range []string{`module "test"`, "metric 0", test.field} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileConfigRejectsReservedMetricName(t *testing.T) {
	for metricName := range reservedProbeMetrics {
		t.Run(metricName, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{
				"test": {Metrics: []Metric{{Name: metricName, Value: "1"}}},
			}})
			if err == nil {
				t.Fatal("expected reserved metric name error")
			}
			for _, part := range []string{`module "test"`, "metric 0", fmt.Sprintf(`metric family %q is reserved`, metricName)} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileMetricNameLiteralDetection(t *testing.T) {
	tests := map[string]struct {
		name    Query
		literal bool
	}{
		"plain name":         {name: "probe_value", literal: true},
		"jq builtin as name": {name: "length", literal: true},
		"name with colons":   {name: "node:cpu_ratio", literal: true},
		"index query":        {name: ".foo", literal: false},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			metric := mustCompileMetric(t, Metric{Name: test.name, Value: "1"})
			assert(t, test.name, metric.name.source)
			assert(t, test.literal, metric.name.code == nil)
		})
	}
}

func TestCompileMetricLabelValueLiteralDetection(t *testing.T) {
	tests := map[string]struct {
		label   Query
		literal bool
	}{
		"jq keyword true":          {label: "true", literal: true},
		"jq keyword null":          {label: "null", literal: true},
		"jq builtin":               {label: "length", literal: true},
		"parse error fallback":     {label: "foo:bar", literal: true},
		"compile error fallback":   {label: "us-east-1", literal: true},
		"index query":              {label: ".DNSName", literal: false},
		"parenthesized jq builtin": {label: "(now)", literal: false},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			metric := mustCompileMetric(t, Metric{
				Name:   "probe_value",
				Labels: map[string]Query{"kind": test.label},
				Value:  "1",
			})
			assert(t, test.label, metric.labels["kind"].source)
			assert(t, test.literal, metric.labels["kind"].code == nil)
		})
	}
}

func TestCompileConfigPreservesLiterals(t *testing.T) {
	metric := mustCompileConfig(t, &Config{Modules: map[string]Module{
		"test": {Metrics: []Metric{{
			Name: "node:cpu_ratio",
			Labels: map[string]Query{
				"kind":    "static",
				"state":   "true",
				"version": "v1.2.3",
			},
			ValueType: valueTypeGauge,
			Value:     "1",
		}}},
	}}).Modules["test"].Metrics[0]

	metricSet := newProbeMetricSet()
	if err := makeMetrics(t.Context(), metricSet, nil, metric); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	metricSet.WritePrometheus(&output)
	assert(t, "node:cpu_ratio{kind=\"static\",state=\"true\",version=\"v1.2.3\"} 1\n", output.String())
}
