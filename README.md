# prometheus-jq-exporter

An alternative Prometheus exporter to [json_exporter](https://github.com/prometheus-community/json_exporter) using [gojq](https://github.com/itchyny/gojq).

## Features

- jq expression
- file transport (`target=file://...`)
- unix socket transport (`target=unix:///path/to/target.sock`)

## Install

```
go install github.com/johejo/prometheus-jq-exporter@latest
```

## Usage

```
$ prometheus-jq-exporter -h
Usage of prometheus-jq-exporter:
  -addr string
        listen addr (default ":9999")
  -config string
        config file path (default "config.yaml")
  -enable-file-transport
        enable file transport
  -enable-unix-socket-transport
        enable unix socket transport
  -expand-env
        expand environment variable in config file
  -expose-metadata
        expose metric metadata (default true)
  -log-level string
        log level (default "info")
  -max-response-body-size int
        maximum target response body size in bytes (default 10485760)
  -read-header-timeout duration
        HTTP server request header read timeout (default 5s)
  -target-timeout duration
        target request timeout (default 30s)

```

Target responses are limited to 10 MiB by default, and the complete target request—including reading its response body—must finish within 30 seconds. Incoming request headers must be received within 5 seconds. Use the corresponding flags to tune these limits; all values must be positive.

## Example

### Unix socket transport

Enable the Unix socket transport and use the `unix` scheme for the probe target:

```
$ prometheus-jq-exporter --enable-unix-socket-transport --config ./testdata/config.yaml
$ curl 'localhost:9999/probe?module=tailscale&target=unix:///path/to/target.sock/status'
```

The socket path ends at the first path segment with a `.sock` suffix. Any remaining path is sent as the HTTP request path; when it is omitted, `/` is used. The former implicit `http:///path/to/target.sock` form is not supported.

Each module can restrict the HTTP response status codes accepted from the target:

```yaml
modules:
  example:
    valid_status_codes: [200, 404]
    metrics:
      # ...
```

When `valid_status_codes` is omitted or empty, only 2xx responses are accepted. When it is set, only the listed status codes are accepted, including non-2xx responses.

### Request body

Use `body.json` or `body.text` to build a request body with a jq expression. The expression receives probe URL query parameters as an object whose values are always arrays of strings. The exporter control parameters `module`, `target`, `method`, and `debug` are excluded. For example, `?query=up&tag=a&tag=b` is available as `{"query":["up"],"tag":["a","b"]}`.

`body.json` serializes the expression result as JSON:

```yaml
modules:
  example:
    body:
      json: |
        {
          query: .query[0],
          tags: .tag
        }
    # ...
```

`body.text` requires the expression to produce a string and sends that string without additional encoding:

```yaml
modules:
  example:
    body:
      text: '"query=\(.query[0] | @uri)"'
    headers:
      Content-Type: application/x-www-form-urlencoded
    # ...
```

Only one of `body.json` and `body.text` can be specified. Each expression must produce exactly one value. The default content types are `application/json` and `text/plain; charset=utf-8`, respectively; an explicit `Content-Type` in `headers` overrides the default.

Each metric accepts `valueType: counter`, `valueType: gauge`, or `valueType: untyped`. When `valueType` is omitted, it defaults to `untyped`. Prefer `counter` or `gauge` when the source metric's semantics are known.

A metric's `query` expression selects the objects that produce samples: every output contributes, and array outputs are flattened one level, so `.items` and `.items[]` are equivalent. No outputs means no samples. The `name`, `labels`, and `value` expressions run once per selected object and must each produce exactly one value.

Set `epochTimestamp` to a jq expression to use a value from each metric object as the sample timestamp. The value must be an integer Unix timestamp in milliseconds that fits in an `int64`. Integer-valued floats and base-10 integer strings are also accepted:

```yaml
metrics:
  - name: example_timestamped_value
    query: '.values'
    valueType: gauge
    value: '.count'
    epochTimestamp: '.timestamp'
```

If the timestamp expression produces `null` or no value, the timestamp is treated as optional and the sample is exposed without one. If the expression cannot be evaluated, produces multiple values, or produces another unsupported value, the sample is exposed without a timestamp, the error is logged, and `probe_success` is set to `0`. Explicit timestamps change Prometheus staleness handling; see the [Prometheus staleness documentation](https://prometheus.io/docs/prometheus/latest/querying/basics/#staleness) before enabling them.

### Probe success

Every completed probe returns HTTP 200 and exposes the following gauges:

- `probe_success`: `1` only when every probe stage succeeds; otherwise `0`.
- `probe_body_errors`: request body evaluation errors.
- `probe_fetch_errors`: target request timeout, accepted status, response size/read, or JSON decoding errors.
- `probe_metrics_successful`: successfully generated samples.
- `probe_metrics_failed`: metric query or sample generation errors.
- `probe_timestamp_errors`: timestamp evaluation or conversion errors. The corresponding sample is still exposed without a timestamp.

All error gauges and `probe_metrics_failed` are counts for the current probe, not cumulative counters. Metric generation is best-effort: valid samples are returned even when another query or sample fails. A partial failure has both `probe_metrics_successful > 0` and `probe_metrics_failed > 0`; an all-metric failure has no successful samples.

This HTTP behavior is a compatibility change from versions that returned HTTP 500 for all-metric or body failures and HTTP 503 for fetch failures. Prometheus should alert on the probe gauges rather than the scrape status. To inspect error details without accessing exporter logs, add `debug=true` to the probe URL; errors are emitted as `# probe_error` comments before the metrics.

Missing `module` or `target` parameters and unknown modules are request errors and return HTTP 400. Use `probe_success == 0` to alert on target or metric generation failures; the Prometheus `up` metric only indicates whether the exporter endpoint itself could be scraped.

The probe gauge names listed above are reserved and cannot be used as configured metric family names.

```
$ prometheus-jq-exporter --config ./testdata/config.yaml
```

```
$ python3 -m http.server -d ./testdata
```

```
$ curl 'localhost:9999/probe?module=tailscale&target=http://localhost:8000/tailscale-status.json'
# HELP probe_body_errors
# TYPE probe_body_errors gauge
probe_body_errors 0
# HELP probe_fetch_errors
# TYPE probe_fetch_errors gauge
probe_fetch_errors 0
# HELP probe_metrics_failed
# TYPE probe_metrics_failed gauge
probe_metrics_failed 0
# HELP probe_metrics_successful
# TYPE probe_metrics_successful gauge
probe_metrics_successful 6
# HELP probe_success
# TYPE probe_success gauge
probe_success 1
# HELP probe_timestamp_errors
# TYPE probe_timestamp_errors gauge
probe_timestamp_errors 0
# HELP tailscale_status_peer
# TYPE tailscale_status_peer gauge
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
# HELP tailscale_status_peer_rx_bytes
# TYPE tailscale_status_peer_rx_bytes gauge
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
# HELP tailscale_status_peer_tx_bytes
# TYPE tailscale_status_peer_tx_bytes gauge
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
```

## License

BSD 3-Clause

## Author

Mitsuo HEIJO
