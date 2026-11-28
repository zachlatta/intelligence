package deploy

import _ "embed"

// InstallHost is the standalone host installer script (deploy/install-host).
//
//go:embed install-host
var InstallHost string
