package exporter

import (
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
	"github.com/itchyny/gojq"
)

func loadConfig(config string, expandEnv bool) (*Config, error) {
	b, err := os.ReadFile(config)
	if err != nil {
		return nil, err
	}

	if expandEnv {
		b = []byte(os.ExpandEnv(string(b)))
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
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
