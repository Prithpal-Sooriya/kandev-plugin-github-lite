package githublite

import (
	"os"

	hclog "github.com/hashicorp/go-hclog"
)

// logger writes hclog-JSON to stderr. kandev's go-plugin client parses those
// lines and re-emits them into the backend log (hclog Info survives the
// adapter's level mapping), so this is the plugin's diagnostic channel — the
// only way an operator can see which URL an action received when a picker or
// task-create flow misbehaves. Keep it Info and terse: one line per action
// invocation, never per GitHub request.
var logger = hclog.New(&hclog.LoggerOptions{
	Name:       "github-lite",
	Output:      os.Stderr,
	Level:       hclog.Info,
	JSONFormat:  true,
})
