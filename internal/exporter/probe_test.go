package exporter

import (
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

func TestReadResponseBodyLimit(t *testing.T) {
	for name, test := range map[string]struct {
		body    string
		maxSize int64
		want    string
		wantErr string
	}{
		"below limit":   {body: "ab", maxSize: 3, want: "ab"},
		"at limit":      {body: "abc", maxSize: 3, want: "abc"},
		"over limit":    {body: "abcd", maxSize: 3, wantErr: "exceeds 3 bytes"},
		"invalid limit": {body: "", maxSize: 0, wantErr: "must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := readResponseBody(strings.NewReader(test.body), test.maxSize)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected %q error, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assert(t, test.want, string(body))
		})
	}
}

func TestProbeResponseBodyLimit(t *testing.T) {
	const response = `{"value":1}`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(target.Close)

	cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {}}})
	query := "/probe?module=test&debug=true&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(cfg, http.DefaultClient, int64(len(response)-1)))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	if !strings.Contains(body, "target response body exceeds 10 bytes") {
		t.Fatalf("debug response does not contain response size error: %q", body)
	}
	if !strings.HasSuffix(body, probeStatus(0, 1, 0, 0, 0, 0)) {
		t.Fatalf("response size error was not recorded as a fetch error: %q", body)
	}
}

func Test(t *testing.T) {
	httpClient := newHTTPClientWithFileRoot(true, true, "../..", defaultTargetTimeout)

	cfg, err := loadConfig("../../testdata/config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	want := probeStatus(0, 0, 0, 6, 1, 0) + trim(`
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
	t.Run("file", func(t *testing.T) {
		result := testReq(http.MethodGet, "/probe?module=tailscale&target=file://testdata/tailscale-status.json", nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		assert(t, want, b)
	})

	t.Run("http", func(t *testing.T) {
		ts := httptest.NewServer(http.FileServer(http.Dir("../../testdata")))
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=%s/tailscale-status.json", ts.URL)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		assert(t, want, b)
	})

	t.Run("unix", func(t *testing.T) {
		testSock := filepath.Join(t.TempDir(), "test.sock")
		ts := httptest.NewUnstartedServer(http.FileServer(http.Dir("../../testdata")))
		ts.Listener = must[net.Listener](t)(net.Listen("unix", testSock))
		ts.Start()
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=unix://%s/tailscale-status.json", testSock)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
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

	probe := handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize)
	type requestResult struct {
		statusCode int
		body       string
		err        error
	}
	request := func(path string) requestResult {
		query := "/probe?module=test&target=" + url.QueryEscape(target.URL+path)
		result := testReq(http.MethodGet, query, nil, probe)
		body, err := io.ReadAll(result.Body)
		return requestResult{statusCode: result.StatusCode, body: string(body), err: err}
	}
	checkRequest := func(path string, result requestResult) string {
		t.Helper()
		if result.err != nil {
			t.Fatalf("read response from %s: %v", path, result.err)
		}
		assert(t, http.StatusOK, result.statusCode)
		return result.body
	}

	metricNamesBefore := strings.Join(metrics.ListMetricNames(), "\n")
	first := checkRequest("/first", request("/first"))
	assert(t, probeStatus(0, 0, 0, 2, 1, 0)+"probe_value{id=\"a\"} 1\nprobe_value{id=\"b\"} 2\n", first)

	second := checkRequest("/second", request("/second"))
	assert(t, probeStatus(0, 0, 0, 1, 1, 0)+"probe_value{id=\"a\"} 10\n", second)
	assert(t, metricNamesBefore, strings.Join(metrics.ListMetricNames(), "\n"))

	t.Run("concurrent targets", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]requestResult, 2)
		paths := []string{"/second", "/third"}
		for i := range paths {
			wg.Go(func() {
				<-start
				results[i] = request(paths[i])
			})
		}
		close(start)
		wg.Wait()

		assert(t, probeStatus(0, 0, 0, 1, 1, 0)+"probe_value{id=\"a\"} 10\n", checkRequest(paths[0], results[0]))
		assert(t, probeStatus(0, 0, 0, 1, 1, 0)+"probe_value{id=\"c\"} 30\n", checkRequest(paths[1], results[1]))
	})
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
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, probeStatus(0, 0, 0, 1, 1, 0)+trim(`
probe_value{label="quote\"line\nbreak\\"} 1
`), body)
}

func TestProbeErrorStatus(t *testing.T) {
	invalidJSONTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not JSON")
	}))
	t.Cleanup(invalidJSONTarget.Close)

	tests := map[string]struct {
		cfg      *Config
		target   string
		want     int
		wantBody string
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
			cfg:      &Config{Modules: map[string]Module{"test": {}}},
			target:   "/probe?module=test&target=%3A%2F%2F",
			want:     http.StatusOK,
			wantBody: probeStatus(0, 1, 0, 0, 0, 0),
		},
		"invalid JSON response": {
			cfg:      &Config{Modules: map[string]Module{"test": {}}},
			target:   "/probe?module=test&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:     http.StatusOK,
			wantBody: probeStatus(0, 1, 0, 0, 0, 0),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := testReq(http.MethodGet, test.target, nil, handleProbe(mustCompileConfig(t, test.cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.want, result.StatusCode)
			if test.wantBody != "" {
				body := string(must[[]byte](t)(io.ReadAll(result.Body)))
				assert(t, test.wantBody, body)
			}
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
			body: Body{JSON: new(`{
  name: .name[0],
  tags: .tag,
  values: [1, true, null]
}`)},
			wantBody:        `{"name":"quote\"line\nbreak","tags":["a","b"],"values":[1,true,null]}`,
			wantContentType: "application/json",
		},
		"json excludes exporter parameters": {
			body:            Body{JSON: new(`.`)},
			wantBody:        `{"name":["quote\"line\nbreak"],"tag":["a","b"]}`,
			wantContentType: "application/json",
		},
		"text": {
			body:            Body{Text: new(`"name=\(.name[0]);tags=\(.tag | join(","))"`)},
			wantBody:        "name=quote\"line\nbreak;tags=a,b",
			wantContentType: "text/plain; charset=utf-8",
		},
		"text excludes exporter parameters": {
			body:            Body{Text: new(`"keys=\(keys | join(","))"`)},
			wantBody:        "keys=name,tag",
			wantContentType: "text/plain; charset=utf-8",
		},
		"empty text": {
			body:            Body{Text: new(`""`)},
			wantContentType: "text/plain; charset=utf-8",
		},
		"content type override": {
			body:            Body{Text: new(`"name=\(.name[0])"`)},
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
				"debug":  {"true"},
				"name":   {"quote\"line\nbreak"},
				"tag":    {"a", "b"},
			}
			result := testReq(http.MethodGet, "/probe?"+params.Encode(), nil, handleProbe(cfg, http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, http.StatusOK, result.StatusCode)
			assert(t, test.wantBody, gotBody)
			assert(t, test.wantContentType, gotContentType)
		})
	}
}

func TestProbeRejectsInvalidBodyResults(t *testing.T) {
	tests := map[string]Body{
		"evaluation error": {JSON: new(`error("failed")`)},
		"no values":        {JSON: new(`empty`)},
		"multiple values":  {JSON: new(`1, 2`)},
		"non-string text":  {Text: new(`1`)},
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
			result := testReq(http.MethodGet, query, nil, handleProbe(cfg, http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, http.StatusOK, result.StatusCode)
			probeBody := string(must[[]byte](t)(io.ReadAll(result.Body)))
			assert(t, probeStatus(1, 0, 0, 0, 0, 0), probeBody)
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
			wantBody:   probeStatus(0, 0, 1, 1, 0, 0) + "successful_metric{} 2\n",
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
			wantBody:   "item_value{id=\"a\"} 1\nitem_value{id=\"c\"} 3\n" + probeStatus(0, 0, 1, 2, 0, 0),
		},
		"duplicate name and labels": {
			body: `{"items":[{"id":"a","value":1},{"id":"a","value":2}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_value{id=\"a\"} 1\n" + probeStatus(0, 0, 1, 1, 0, 0),
		},
		"all values fail": {
			body: `{"items":[{"value":"invalid"}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"negative counter": {
			body: `{"value":-1}`,
			metrics: []Metric{{
				Name:      "item_count",
				ValueType: valueTypeCounter,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"large counter": {
			body: `{"value":10000000}`,
			metrics: []Metric{{
				Name:      "item_count",
				ValueType: valueTypeCounter,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_count{} 10000000\n" + probeStatus(0, 0, 0, 1, 1, 0),
		},
		"null metric query": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "null_input",
				Query:     ".missing",
				ValueType: valueTypeGauge,
				Value:     "1",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "null_input{} 1\n" + probeStatus(0, 0, 0, 1, 1, 0),
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
			wantBody:   probeStatus(0, 0, 0, 0, 1, 0),
		},
		"iterator query": {
			body: `{"items":[{"id":"a","value":1},{"id":"b","value":2}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items[]",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_value{id=\"a\"} 1\nitem_value{id=\"b\"} 2\n" + probeStatus(0, 0, 0, 2, 1, 0),
		},
		"multiple array outputs": {
			body: `{"a":[{"id":"a","value":1}],"b":[{"id":"b","value":2}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".a, .b",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_value{id=\"a\"} 1\nitem_value{id=\"b\"} 2\n" + probeStatus(0, 0, 0, 2, 1, 0),
		},
		"empty query output": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     "empty",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 0, 0, 1, 0),
		},
		"value multiple outputs": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				ValueType: valueTypeGauge,
				Value:     "1, 2",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"value no output": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				ValueType: valueTypeGauge,
				Value:     "empty",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"label multiple outputs": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				Labels:    map[string]Query{"id": "1, 2"},
				ValueType: valueTypeGauge,
				Value:     "1",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"timestamp multiple outputs": {
			body: `{"value":1,"ts":1712345678901}`,
			metrics: []Metric{{
				Name:           "ts_value",
				ValueType:      valueTypeGauge,
				Value:          ".value",
				EpochTimestamp: ".ts, .ts",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 0, 1, 0, 1) + "ts_value{} 1\n",
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
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.wantStatus, result.StatusCode)
			body := string(must[[]byte](t)(io.ReadAll(result.Body)))
			assert(t, test.wantBody, body)
		})
	}
}

func TestProbeRejectsDynamicReservedMetricName(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"probe_success","value":1}`)
	}))
	t.Cleanup(target.Close)

	cfg := &Config{Modules: map[string]Module{
		"test": {Metrics: []Metric{{Name: ".name", ValueType: valueTypeGauge, Value: ".value"}}},
	}}
	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, probeStatus(0, 0, 1, 0, 0, 0), body)
}

func TestProbeDebugIncludesError(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{"test": {}}}
	result := testReq(http.MethodGet, "/probe?module=test&debug=true&target=%3A%2F%2F", nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	if !strings.Contains(body, `# probe_error "parse \"://\": missing protocol scheme"`+"\n") {
		t.Fatalf("debug response does not contain fetch error: %q", body)
	}
	if !strings.HasSuffix(body, probeStatus(0, 1, 0, 0, 0, 0)) {
		t.Fatalf("debug response does not contain probe metrics: %q", body)
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
		wantSuccess      int
	}{
		"default accepts 2xx": {
			status:      299,
			want:        http.StatusOK,
			wantSuccess: 1,
		},
		"default rejects non-2xx": {
			status: http.StatusNotFound,
			want:   http.StatusOK,
		},
		"configured accepts non-2xx": {
			status:           http.StatusNotFound,
			validStatusCodes: []int{http.StatusNotFound},
			want:             http.StatusOK,
			wantSuccess:      1,
		},
		"configured rejects unlisted 2xx": {
			status:           http.StatusOK,
			validStatusCodes: []int{http.StatusCreated},
			want:             http.StatusOK,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Modules: map[string]Module{
				"test": {ValidStatusCodes: test.validStatusCodes},
			}}
			probeTarget := fmt.Sprintf("%s?status=%d", target.URL, test.status)
			query := "/probe?module=test&target=" + url.QueryEscape(probeTarget)
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.want, result.StatusCode)
			body := string(must[[]byte](t)(io.ReadAll(result.Body)))
			fetchErrors := 0
			if test.wantSuccess == 0 {
				fetchErrors = 1
			}
			assert(t, probeStatus(0, fetchErrors, 0, 0, test.wantSuccess, 0), body)
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
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, probeStatus(0, 0, 0, 3, 1, 0)+trim(`
probe_value{id="a"} 1 1712345678901
probe_value{id="b"} 2 1712345678902
probe_value{id="c"} 3
`), body)
}

func TestProbeMetricFloatValueFormatting(t *testing.T) {
	tests := map[string]struct {
		value     float64
		valueType string
		want      string
	}{
		"gauge at two to the power of 63": {
			value:     math.Exp2(63),
			valueType: valueTypeGauge,
			want:      "probe_value{} 9.223372036854776e+18\n",
		},
		"largest float below two to the power of 63": {
			value:     math.Nextafter(math.Exp2(63), 0),
			valueType: valueTypeGauge,
			want:      "probe_value{} 9223372036854774784\n",
		},
		"minimum int64": {
			value:     math.MinInt64,
			valueType: valueTypeGauge,
			want:      "probe_value{} -9223372036854775808\n",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			metricSet.metrics["probe_value{}"] = probeMetric{
				family:     "probe_value",
				name:       "probe_value{}",
				valueType:  test.valueType,
				floatValue: test.value,
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WritePrometheus(&output)
			assert(t, test.want, output.String())
		})
	}
}
