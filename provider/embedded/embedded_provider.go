//go:build llamacpp

package embedded

// embedded_provider.go — the CGo-enabled embedded provider.
//
// When built with the `llamacpp` tag, this file takes precedence over
// embedded_stub.go. In the future this will use native CGo bindings to
// llama.cpp for zero-copy in-process inference. For now it delegates to the
// same subprocess-based path as the stub (ProcessManager + OpenAI client) so
// the provider is functional regardless of build tag.
//
// The Provider type and all methods are defined in embedded_stub.go (which
// is excluded by this build tag). To avoid a duplicate-type error, this file
// only provides the CGo-specific constructor; the real logic lives in
// embedded_stub.go and is compiled when the tag is OFF.
//
// When native CGo bindings are added, replace NewProvider here with a version
// that initializes the CGo handle and implement CreateClient to use it
// directly instead of spawning a subprocess.

import "github.com/darkcode/scheduler"

// NewProvider is the CGo-tagged constructor. It delegates to the same
// ProcessManager-based implementation. When CGo bindings land, override here.
func NewProvider(scheduler *scheduler.Scheduler) *Provider {
	return NewProviderWithDirs(scheduler, "", "")
}
