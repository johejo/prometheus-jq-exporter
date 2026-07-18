package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VictoriaMetrics/metrics"
	"github.com/itchyny/gojq"
)

func Test(t *testing.T) {
	*enableFileTransport = true
	*enableUnixSocketTransport = true
	t.Cleanup(func() {
		*enableFileTransport = false
		*enableUnixSocketTransport = false
	})

	cfg, err := loadConfig("./testdata/config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("file", func(t *testing.T) {
		result := testReq(http.MethodGet, "/probe?module=tailscale&target=file://testdata/tailscale-status.json", nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
		assert(t, want, b)
	})

	t.Run("http", func(t *testing.T) {
		ts := httptest.NewServer(http.FileServer(http.Dir("./testdata")))
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=%s/tailscale-status.json", ts.URL)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
		assert(t, want, b)
	})

	t.Run("unix", func(t *testing.T) {
		testSock := filepath.Join(t.TempDir(), "test.sock")
		ts := httptest.NewUnstartedServer(http.FileServer(http.Dir("./testdata")))
		ts.Listener = must[net.Listener](t)(net.Listen("unix", testSock))
		ts.Start()
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=http://%s/tailscale-status.json", testSock)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
		assert(t, want, b)
	})
}

func TestProbeMetricsAreRequestScoped(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:      "probe_value",
					Query:     ".items",
					Labels:    map[string]Query{"id": ".id"},
					ValueType: "gauge",
					Value:     ".value",
				},
			},
		},
	}}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first":
			_, _ = io.WriteString(w, `{"items":[{"id":"a","value":1},{"id":"b","value":2}]}`)
		case "/second":
			_, _ = io.WriteString(w, `{"items":[{"id":"a","value":10}]}`)
		case "/third":
			_, _ = io.WriteString(w, `{"items":[{"id":"c","value":30}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(target.Close)

	probe := handleProbe(mustCompileConfig(t, cfg))
	request := func(path string) string {
		t.Helper()
		query := "/probe?module=test&target=" + url.QueryEscape(target.URL+path)
		result := testReq(http.MethodGet, query, nil, probe)
		assert(t, http.StatusOK, result.StatusCode)
		return string(must[[]byte](t)(io.ReadAll(result.Body)))
	}

	metricNamesBefore := strings.Join(metrics.ListMetricNames(), "\n")
	first := request("/first")
	assert(t, trim(`
probe_value{id="a"} 1
probe_value{id="b"} 2
`), first)

	second := request("/second")
	assert(t, trim(`
probe_value{id="a"} 10
`), second)
	assert(t, metricNamesBefore, strings.Join(metrics.ListMetricNames(), "\n"))

	t.Run("concurrent targets", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]string, 2)
		paths := []string{"/second", "/third"}
		for i := range paths {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = request(paths[i])
			}()
		}
		close(start)
		wg.Wait()

		assert(t, trim(`
probe_value{id="a"} 10
`), results[0])
		assert(t, trim(`
probe_value{id="c"} 30
`), results[1])
	})
}

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

func TestProbeEscapesLabelValues(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:      "probe_value",
					Labels:    map[string]Query{"label": ".label"},
					ValueType: "gauge",
					Value:     ".value",
				},
			},
		},
	}}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"label":"quote\"line\nbreak\\","value":1}`)
	}))
	t.Cleanup(target.Close)

	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg)))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, "probe_value{label=\"quote\\\"line\\nbreak\\\\\"} 1\n", body)
}

func TestProbeErrorStatus(t *testing.T) {
	invalidJSONTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not JSON")
	}))
	t.Cleanup(invalidJSONTarget.Close)
	validJSONTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(validJSONTarget.Close)

	tests := map[string]struct {
		cfg    *Config
		target string
		want   int
	}{
		"missing module": {
			cfg:    &Config{},
			target: "/probe?target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusBadRequest,
		},
		"unknown module": {
			cfg:    &Config{},
			target: "/probe?module=unknown&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusBadRequest,
		},
		"missing target": {
			cfg:    &Config{Modules: map[string]Module{"test": {}}},
			target: "/probe?module=test",
			want:   http.StatusBadRequest,
		},
		"invalid target": {
			cfg:    &Config{Modules: map[string]Module{"test": {}}},
			target: "/probe?module=test&target=%3A%2F%2F",
			want:   http.StatusServiceUnavailable,
		},
		"invalid JSON response": {
			cfg:    &Config{Modules: map[string]Module{"test": {}}},
			target: "/probe?module=test&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusServiceUnavailable,
		},
		"body query evaluation failure": {
			cfg: &Config{Modules: map[string]Module{"test": {
				Body: Body{JSON: queryPointer(`error("failed")`)},
			}}},
			target: "/probe?module=test&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusInternalServerError,
		},
		"jq query evaluation failure": {
			cfg: &Config{Modules: map[string]Module{"test": {
				Metrics: []Metric{{Name: "failed_metric", Query: `error("failed")`, Value: "1"}},
			}}},
			target: "/probe?module=test&target=" + url.QueryEscape(validJSONTarget.URL),
			want:   http.StatusInternalServerError,
		},
		"invalid metric": {
			cfg: &Config{Modules: map[string]Module{"test": {
				Metrics: []Metric{{Name: `"invalid-name"`, ValueType: "gauge", Value: "1"}},
			}}},
			target: "/probe?module=test&target=" + url.QueryEscape(validJSONTarget.URL),
			want:   http.StatusInternalServerError,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := testReq(http.MethodGet, test.target, nil, handleProbe(mustCompileConfig(t, test.cfg)))
			assert(t, test.want, result.StatusCode)
		})
	}
}

func TestProbeBody(t *testing.T) {
	tests := map[string]struct {
		body            Body
		headers         map[string]string
		wantBody        string
		wantContentType string
	}{
		"none": {},
		"json": {
			body: Body{JSON: queryPointer(`{
  name: .name[0],
  tags: .tag,
  values: [1, true, null]
}`)},
			wantBody:        `{"name":"quote\"line\nbreak","tags":["a","b"],"values":[1,true,null]}`,
			wantContentType: "application/json",
		},
		"text": {
			body:            Body{Text: queryPointer(`"name=\(.name[0]);tags=\(.tag | join(","))"`)},
			wantBody:        "name=quote\"line\nbreak;tags=a,b",
			wantContentType: "text/plain; charset=utf-8",
		},
		"empty text": {
			body:            Body{Text: queryPointer(`""`)},
			wantContentType: "text/plain; charset=utf-8",
		},
		"content type override": {
			body:            Body{Text: queryPointer(`"name=\(.name[0])"`)},
			headers:         map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			wantBody:        "name=quote\"line\nbreak",
			wantContentType: "application/x-www-form-urlencoded",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var gotBody, gotContentType string
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = string(must[[]byte](t)(io.ReadAll(r.Body)))
				gotContentType = r.Header.Get("Content-Type")
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(target.Close)

			cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{
				"test": {Body: test.body, Headers: test.headers},
			}})
			params := url.Values{
				"module": {"test"},
				"target": {target.URL},
				"method": {http.MethodPost},
				"name":   {"quote\"line\nbreak"},
				"tag":    {"a", "b"},
			}
			result := testReq(http.MethodGet, "/probe?"+params.Encode(), nil, handleProbe(cfg))
			assert(t, http.StatusOK, result.StatusCode)
			assert(t, test.wantBody, gotBody)
			assert(t, test.wantContentType, gotContentType)
		})
	}
}

func TestProbeRejectsInvalidBodyResults(t *testing.T) {
	tests := map[string]Body{
		"evaluation error": {JSON: queryPointer(`error("failed")`)},
		"no values":        {JSON: queryPointer(`empty`)},
		"multiple values":  {JSON: queryPointer(`1, 2`)},
		"non-string text":  {Text: queryPointer(`1`)},
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			targetCalled := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetCalled = true
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(target.Close)

			cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {Body: body}}})
			query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
			result := testReq(http.MethodGet, query, nil, handleProbe(cfg))
			assert(t, http.StatusInternalServerError, result.StatusCode)
			if targetCalled {
				t.Fatal("target was called after body evaluation failed")
			}
		})
	}
}

func TestProbeMetricGenerationPartialFailure(t *testing.T) {
	tests := map[string]struct {
		body       string
		metrics    []Metric
		wantStatus int
		wantBody   string
	}{
		"metric query failure": {
			body: `{}`,
			metrics: []Metric{
				{Name: "failed_metric", Query: `error("failed")`, ValueType: valueTypeGauge, Value: "1"},
				{Name: "successful_metric", ValueType: valueTypeGauge, Value: "2"},
			},
			wantStatus: http.StatusOK,
			wantBody:   "successful_metric{} 2\n",
		},
		"value failure": {
			body: `{"items":[{"id":"a","value":1},{"id":"b","value":"invalid"},{"id":"c","value":3}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody: trim(`
item_value{id="a"} 1
item_value{id="c"} 3
`),
		},
		"all values fail": {
			body: `{"items":[{"value":"invalid"}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "Internal Server Error\n",
		},
		"empty values": {
			body: `{"items":[]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(target.Close)

			cfg := &Config{Modules: map[string]Module{
				"test": {Metrics: test.metrics},
			}}
			query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg)))
			assert(t, test.wantStatus, result.StatusCode)
			body := string(must[[]byte](t)(io.ReadAll(result.Body)))
			assert(t, test.wantBody, body)
		})
	}
}

func TestProbeValidStatusCodes(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := must[int](t)(strconv.Atoi(r.URL.Query().Get("status")))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(target.Close)

	tests := map[string]struct {
		status           int
		validStatusCodes []int
		want             int
	}{
		"default accepts 2xx": {
			status: 299,
			want:   http.StatusOK,
		},
		"empty list accepts 2xx": {
			status:           http.StatusOK,
			validStatusCodes: []int{},
			want:             http.StatusOK,
		},
		"default rejects non-2xx": {
			status: http.StatusNotFound,
			want:   http.StatusServiceUnavailable,
		},
		"configured accepts non-2xx": {
			status:           http.StatusNotFound,
			validStatusCodes: []int{http.StatusNotFound},
			want:             http.StatusOK,
		},
		"configured rejects unlisted 2xx": {
			status:           http.StatusOK,
			validStatusCodes: []int{http.StatusCreated},
			want:             http.StatusServiceUnavailable,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Modules: map[string]Module{
				"test": {ValidStatusCodes: test.validStatusCodes},
			}}
			probeTarget := fmt.Sprintf("%s?status=%d", target.URL, test.status)
			query := "/probe?module=test&target=" + url.QueryEscape(probeTarget)
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg)))
			assert(t, test.want, result.StatusCode)
		})
	}
}

func TestLoadConfigValidStatusCodes(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    valid_status_codes: [200, 404]\n",
		"json": `{"modules":{"test":{"valid_status_codes":[200,404]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, []int{http.StatusOK, http.StatusNotFound}, cfg.Modules["test"].ValidStatusCodes)
		})
	}
}

func TestLoadConfigDefaultsValueTypeToUntyped(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    metrics:\n      - name: test_metric\n        value: 1\n",
		"json": `{"modules":{"test":{"metrics":[{"name":"test_metric","value":"1"}]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, valueTypeUntyped, cfg.Modules["test"].Metrics[0].ValueType)
		})
	}
}

func TestLoadConfigEpochTimestamp(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    metrics:\n      - name: test_metric\n        value: 1\n        epochTimestamp: .timestamp\n",
		"json": `{"modules":{"test":{"metrics":[{"name":"test_metric","value":"1","epochTimestamp":".timestamp"}]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, Query(".timestamp"), cfg.Modules["test"].Metrics[0].EpochTimestamp)
		})
	}
}

func TestLoadConfigBody(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    body:\n      json: '{value: .value[0]}'\n",
		"json": `{"modules":{"test":{"body":{"text":"\"value=\\(.value[0])\""}}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			body := must[*Config](t)(loadConfig(configPath, false)).Modules["test"].Body
			if body.query == nil {
				t.Fatal("body query was not compiled")
			}
			wantFormat := bodyFormatJSON
			if extension == "json" {
				wantFormat = bodyFormatText
			}
			assert(t, wantFormat, body.format)
		})
	}
}

func TestCompileConfigCompilesAndReusesQueries(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Body: Body{JSON: queryPointer(".value")},
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
			body: Body{JSON: queryPointer("."), Text: queryPointer(".")},
			part: "mutually exclusive",
		},
		"empty json": {
			body: Body{JSON: queryPointer("")},
			part: "json query is empty",
		},
		"empty text": {
			body: Body{Text: queryPointer("")},
			part: "text query is empty",
		},
		"json parse error": {
			body: Body{JSON: queryPointer("(")},
			part: "json",
		},
		"text compile error": {
			body: Body{Text: queryPointer("unknown_function")},
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
		"label parse error": {
			metric: Metric{Name: "metric", Labels: map[string]Query{"label": "("}, Value: "1"},
			field:  `label "label"`,
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

func TestCompileConfigPreservesLiteralFallback(t *testing.T) {
	metric := mustCompileConfig(t, &Config{Modules: map[string]Module{
		"test": {Metrics: []Metric{{
			Name:      "probe_value",
			Labels:    map[string]Query{"kind": "static"},
			ValueType: valueTypeGauge,
			Value:     "1",
		}}},
	}}).Modules["test"].Metrics[0]

	if metric.name.code != nil || metric.labels["kind"].code != nil {
		t.Fatal("literal name and label should not have executable jq code")
	}
	metricSet := newProbeMetricSet()
	if err := makeMetrics(t.Context(), metricSet, nil, metric); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	metricSet.WritePrometheus(&output)
	assert(t, "probe_value{kind=\"static\"} 1\n", output.String())
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

func TestProbeEpochTimestampPerValue(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:           "probe_value",
					Query:          ".items",
					Labels:         map[string]Query{"id": ".id"},
					ValueType:      valueTypeGauge,
					Value:          ".value",
					EpochTimestamp: ".timestamp",
				},
			},
		},
	}}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":"a","value":1,"timestamp":1712345678901},{"id":"b","value":2,"timestamp":"1712345678902"},{"id":"c","value":3}]}`)
	}))
	t.Cleanup(target.Close)

	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg)))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, trim(`
probe_value{id="a"} 1 1712345678901
probe_value{id="b"} 2 1712345678902
probe_value{id="c"} 3
`), body)
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
		"missing value":          {query: ".timestamp", value: map[string]any{}},
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
			metricSet.WritePrometheus(&output)
			assert(t, "probe_value{} 1\n", output.String())
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

func trim(s string) string {
	return strings.TrimPrefix(s, "\n")
}

func mustCompileConfig(t *testing.T, cfg *Config) *Config {
	t.Helper()
	if err := compileConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func queryPointer(query Query) *Query {
	return &query
}

func mustCompileMetric(t *testing.T, metric Metric) Metric {
	t.Helper()
	compiled, err := compileMetric(make(queryCompiler), metric)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func testReq(method string, target string, body io.Reader, handler http.Handler) *http.Response {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, body)
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func assert[T any](t *testing.T, want T, got T) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}

	wantText := fmt.Sprint(want)
	gotText := fmt.Sprint(got)
	if wantText == gotText {
		t.Errorf("values differ but their textual representations are identical:\nwant (%T): %#v\ngot  (%T): %#v", want, want, got, got)
		return
	}

	var commands [][]string
	if gitPath, err := exec.LookPath("git"); err == nil {
		commands = append(commands, []string{gitPath, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--", "want", "got"})
	}
	if diffPath, err := exec.LookPath("diff"); err == nil {
		commands = append(commands, []string{diffPath, "-u", "--label", "want", "--label", "got", "want", "got"})
	}
	if len(commands) == 0 {
		t.Error("values differ; git and diff are unavailable")
		return
	}

	dir := t.TempDir()
	for name, text := range map[string]string{"want": wantText, "got": gotText} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatalf("write %s for diff: %v", name, err)
		}
	}

	for _, command := range commands {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			_, _ = t.Output().Write(out)
			t.Fail()
			return
		}
	}
	t.Error("values differ; failed to generate diff")
}

func must[T any](t *testing.T) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}
