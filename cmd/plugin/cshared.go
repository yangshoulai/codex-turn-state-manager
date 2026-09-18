//go:build cshared

// Command plugin builds the CPA plugin shared library.
//
// Build with:
//
//	make build-shared
//
// The library is loaded by CPA, which calls cliproxy_plugin_init to hand over
// its host API and receive the plugin's call table. main itself does nothing:
// a c-shared library has no entry point of its own, and the process belongs to
// the host.
package main

import (
	// Importing the adapter for its exported C symbols. The blank import would
	// not be enough -- the linker drops a package whose symbols are only
	// reached from C -- so it is referenced below.
	_ "github.com/yangshoulai/codex-turn-state-manager/internal/pluginabi"
)

func main() {}
