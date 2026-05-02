package agents

import "os"

// Indirection so the manifest-gating test file stays focused on
// intent. These bind to the stdlib functions.
var (
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
	osRemoveAll = os.RemoveAll
)
