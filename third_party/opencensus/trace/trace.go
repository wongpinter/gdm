// Package trace is a minimal local stub of go.opencensus.io/trace,
// covering only the small surface anacrolix/missinggo/v2/httpmux calls
// (StartSpan/WithSpanKind/SpanKindServer/Span.End). See
// third_party/errgo.v2 for why this exists: the real module sits
// behind a host this sandbox can't reach, and we've verified this is
// the only call site in the entire dependency tree that needs it.
package trace

import "context"

type SpanKind int

const SpanKindServer SpanKind = 1

type StartOption func(*startOptions)

type startOptions struct{ kind SpanKind }

func WithSpanKind(kind SpanKind) StartOption {
	return func(o *startOptions) { o.kind = kind }
}

type Span struct{}

func (s *Span) End() {}

func StartSpan(ctx context.Context, name string, opts ...StartOption) (context.Context, *Span) {
	return ctx, &Span{}
}
