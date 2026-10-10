// Package otlpexport constructs the pinned official exporter at process setup.
package otlpexport

import (
	"context"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// New has no connection/readiness handshake and no environment headers, proxy,
// redirects, retries, or unbounded collector response. Each batch is best effort.
func New(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	client := &http.Client{Timeout: time.Second, Transport: boundedTransport{base: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithHeaders(map[string]string{}), otlptracehttp.WithCompression(otlptracehttp.NoCompression), otlptracehttp.WithTimeout(time.Second), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}), otlptracehttp.WithHTTPClient(client))
}

type boundedTransport struct{ base http.RoundTripper }

func (transport boundedTransport) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (transport boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil {
		response.Body = &limitedBody{Reader: io.LimitReader(response.Body, 1<<20), closer: response.Body}
	}
	return response, err
}

type limitedBody struct {
	io.Reader
	closer io.Closer
}

func (body *limitedBody) Close() error { return body.closer.Close() }
