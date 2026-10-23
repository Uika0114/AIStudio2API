//go:build !windows

package authhelper

import "context"

func localStopContext(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func signalLocalStop(int) error { return failure("UNSUPPORTED_KEY") }
