package api

import "context"

// RequestGuard runs before any request is sent and holds its release function
// until the complete logical call, including reads and retries, has finished.
type RequestGuard func(context.Context) (release func(), err error)

func WithRequestGuard(guard RequestGuard) Option {
	return func(client *Client) { client.requestGuard = guard }
}

// RequestGuardError proves that this call sent no HTTP request.
type RequestGuardError struct{ Err error }

func (err *RequestGuardError) Error() string { return "request blocked by credential guard" }
func (err *RequestGuardError) Unwrap() error { return err.Err }
