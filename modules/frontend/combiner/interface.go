package combiner

import (
	"net/http"
)

// Combiner is used to merge multiple responses into a single response.
//
// Implementations must be thread-safe.
// TODO: StatusCode() is only used for multi-tenant support. Can we remove it?
type Combiner interface {
	AddResponse(r PipelineResponse) error
	StatusCode() int
	ShouldQuit() bool

	// returns the final/complete results
	HTTPFinal() (*http.Response, error)
}

type TypedCombiner[T TResponse] interface {
	AddTypedResponse(r T) error
}

type GRPCCombiner[T TResponse] interface {
	Combiner
	TypedCombiner[T]

	GRPCFinal() (T, error)
	GRPCDiff() (T, error)
}
