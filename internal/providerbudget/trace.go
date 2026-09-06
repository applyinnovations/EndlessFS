package providerbudget

import "context"

// Trace attributes provider traffic to one logical operation and subsystem.
// ParallelGroup is shared only by requests intentionally issued concurrently;
// the economics model uses the slowest request in a group for critical-path
// estimates while retaining every request for count and cost.
type Trace struct {
	Operation     string
	Subsystem     string
	ParallelGroup string
}

type traceContextKey struct{}

func WithTrace(ctx context.Context, trace Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceContextKey{}, trace)
}

func TraceFromContext(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	trace, _ := ctx.Value(traceContextKey{}).(Trace)
	return trace
}
