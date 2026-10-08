// Command kandev-plugin-github-lite is the backend half of this plugin.
// It implements pluginsdk.Plugin (see plugin.go) and is spawned by kandev as
// a gRPC subprocess — pluginsdk.Serve owns the entire transport.
package main

import "github.com/kandev/kandev/pkg/pluginsdk"

func main() {
	pluginsdk.Serve(newPlugin())
}
