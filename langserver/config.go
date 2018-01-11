package langserver

import "os"

var (
	// GOLSP_WARMUP_ON_INITIALIZE toggles if we typecheck the whole
	// workspace in the background on initialize. This trades off initial
	// CPU and memory to hide perceived latency of the first few
	// requests. If the LSP server is long lived the tradeoff is usually
	// worth it.
	envWarmupOnInitialize = os.Getenv("GOLSP_WARMUP_ON_INITIALIZE")
)

type Config struct {
	// MaxParallelism controls the maximum number of goroutines that should be used
	// to fulfill requests. This is useful in editor environments where users do
	// not want results ASAP, but rather just semi quickly without eating all of
	// their CPU.
	MaxParallelism int
}
