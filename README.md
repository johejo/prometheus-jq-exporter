# prometheus-jq-exporter

An alternative promethus exporter to [json_exporter](github.com/prometheus-community/json_exporter) using [gojq](https://github.com/itchyny/gojq).

## Features

- jq expression
- file transport (`target=file://...`)
- unix socket transport (`target=http:///path/to/target.sock`)

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

```

## Example


```
$ prometheus-jq-exporter --config ./testdata/config.yaml
```

```
$ python3 -m http.server ./testdata
```

```
$ curl 'localhost:9999/probe?module=tailscale&target=http://localhost:8000/tailscale-status.json'
# HELP tailscale_status_peer_rx_bytes
# TYPE tailscale_status_peer_rx_bytes gauge
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
# HELP tailscale_status_peer_tx_bytes
# TYPE tailscale_status_peer_tx_bytes gauge
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
# HELP tailscale_status_peer
# TYPE tailscale_status_peer gauge
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
```

## License

BSD 3-Clause

## Author

Mitsuo HEIJO
