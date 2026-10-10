package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTelemetryConfigurationIsExplicitPrivateAndNonAuthoritative(t *testing.T) {
	values := map[string]string{"ENDLESSFS_DIAGNOSTICS_ADDR": "127.0.0.1:0", "ENDLESSFS_DIAGNOSTICS_TOKEN": testSecret, "ENDLESSFS_OTLP_TRACES_ENDPOINT": "http://alloy.alloy.svc.cluster.local:4318/v1/traces", "ENDLESSFS_TRACE_SAMPLE_RATIO": "0.25"}
	cfg, err := Parse(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.SampleRatio != .25 || cfg.Telemetry.DiagnosticsToken.Reveal() != testSecret {
		t.Fatal("validated options missing")
	}
	body, err := json.Marshal(cfg.Public())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{testSecret, "diagnostics", "alloy", "sampleRatio"} {
		if strings.Contains(string(body), value) {
			t.Fatalf("private telemetry configuration leaked: %s", value)
		}
	}
	for _, test := range []struct{ key, value string }{
		{"ENDLESSFS_DIAGNOSTICS_ADDR", ":9090"}, {"ENDLESSFS_DIAGNOSTICS_ADDR", "localhost:99999"}, {"ENDLESSFS_DIAGNOSTICS_TOKEN", ""},
		{"ENDLESSFS_TRACE_SAMPLE_RATIO", "NaN"}, {"ENDLESSFS_TRACE_SAMPLE_RATIO", "Inf"}, {"ENDLESSFS_TRACE_SAMPLE_RATIO", "1.1"},
		{"ENDLESSFS_OTLP_TRACES_ENDPOINT", "http://user:secret@collector/v1/traces"}, {"ENDLESSFS_OTLP_TRACES_ENDPOINT", "http://collector/v1/traces?token=secret"}, {"ENDLESSFS_OTLP_TRACES_ENDPOINT", "http://collector"},
	} {
		copied := map[string]string{}
		for key, value := range values {
			copied[key] = value
		}
		copied[test.key] = test.value
		if _, err := Parse(mapLookup(copied)); err == nil {
			t.Errorf("accepted %s=%s", test.key, test.value)
		}
	}
}
