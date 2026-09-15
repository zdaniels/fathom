// Package streamtext carries optional public response text through the runtime.
// Providers must never emit private reasoning or tool arguments here.
package streamtext

import "context"

type key struct{}

func With(ctx context.Context, fn func(string)) context.Context {
	return context.WithValue(ctx, key{}, fn)
}
func Enabled(ctx context.Context) bool { fn, _ := ctx.Value(key{}).(func(string)); return fn != nil }
func Emit(ctx context.Context, text string) {
	if text != "" {
		if fn, _ := ctx.Value(key{}).(func(string)); fn != nil {
			fn(text)
		}
	}
}
