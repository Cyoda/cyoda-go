// Package memory is the in-process storage plugin for cyoda-go.
//
// It serves both as the default backend (no external dependencies)
// and as the minimal reference implementation for plugin authors —
// it implements every required interface method, nothing more.
//
// Registration happens at init() time via spi.Register. A binary
// picks up this plugin by blank-importing it:
//
//	import _ "github.com/cyoda/cyoda-go/plugins/memory"
//
// Configuration: none. The plugin reads no environment variables
// and does not implement spi.DescribablePlugin.
package memory
