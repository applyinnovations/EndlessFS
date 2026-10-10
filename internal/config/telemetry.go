package config

import (
	"fmt"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Telemetry struct {
	DiagnosticsAddr  string
	DiagnosticsToken secret.Value
	TraceEndpoint    string
	SampleRatio      float64
}

func parseTelemetry(lookup func(string) (string, bool)) (Telemetry, error) {
	options := Telemetry{SampleRatio: .1}
	options.DiagnosticsAddr, _ = lookup("ENDLESSFS_DIAGNOSTICS_ADDR")
	options.TraceEndpoint, _ = lookup("ENDLESSFS_OTLP_TRACES_ENDPOINT")
	options.DiagnosticsAddr = strings.TrimSpace(options.DiagnosticsAddr)
	options.TraceEndpoint = strings.TrimSpace(options.TraceEndpoint)
	var err error
	options.DiagnosticsToken, err = parseOptionalBearer(lookup, "ENDLESSFS_DIAGNOSTICS_TOKEN")
	if err != nil {
		return Telemetry{}, err
	}
	if options.DiagnosticsAddr != "" {
		host, port, parseErr := net.SplitHostPort(options.DiagnosticsAddr)
		number, numberErr := strconv.Atoi(port)
		if parseErr != nil || numberErr != nil || number < 0 || number > 65535 || host == "" {
			return Telemetry{}, fmt.Errorf("ENDLESSFS_DIAGNOSTICS_ADDR: expected explicit host:port")
		}
		if options.DiagnosticsToken.Reveal() == "" {
			return Telemetry{}, fmt.Errorf("ENDLESSFS_DIAGNOSTICS_TOKEN: required for diagnostics")
		}
	}
	if options.TraceEndpoint != "" {
		endpoint, parseErr := url.Parse(options.TraceEndpoint)
		if parseErr != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/v1/traces" || endpoint.RawPath != "" {
			return Telemetry{}, fmt.Errorf("ENDLESSFS_OTLP_TRACES_ENDPOINT: expected HTTP(S) collector /v1/traces without credentials, query, or fragment")
		}
	}
	if value, ok := lookup("ENDLESSFS_TRACE_SAMPLE_RATIO"); ok {
		options.SampleRatio, err = strconv.ParseFloat(value, 64)
		if err != nil || !(options.SampleRatio >= 0 && options.SampleRatio <= 1) {
			return Telemetry{}, fmt.Errorf("ENDLESSFS_TRACE_SAMPLE_RATIO: expected finite value from zero to one")
		}
	}
	return options, nil
}
