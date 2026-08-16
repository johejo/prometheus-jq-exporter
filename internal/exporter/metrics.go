package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/VictoriaMetrics/metrics"
	"github.com/itchyny/gojq"
)

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
	if _, exists := metricSet.metrics[metricName]; exists {
		return fmt.Errorf("duplicate metric %q", metricName)
	}
	metricSet.metrics[metricName] = probeMetric
	metricSet.metricsSuccessful++
	return nil
}
