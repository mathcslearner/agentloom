package log

import (
	"context"
	"log/slog"
)

// ctxKey is the private context key for the carried logger.
type ctxKey struct{}

// Into returns a context carrying logger. A nil context is treated as
// context.Background(); a nil logger leaves the context unchanged.
func Into(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, logger)
}

// From returns the logger carried by ctx. It is nil-safe: if ctx is nil or
// carries no logger, it returns slog.Default(), never nil.
func From(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if logger, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}

// With returns a context whose carried logger is the current one extended
// with args (same semantics as slog.Logger.With). Use it to stamp fields
// like run_id once and have every downstream log line inherit them.
func With(ctx context.Context, args ...any) context.Context {
	return Into(ctx, From(ctx).With(args...))
}
