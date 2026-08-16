package main

import (
	"log"

	"github.com/johejo/prometheus-jq-exporter/internal/exporter"
)

var version string

func main() {
	if err := exporter.Run(version); err != nil {
		log.Fatal(err)
	}
}
