#!/usr/bin/env bash
# Hook of the kit's release workflow: runs before the pin commit, with
# plugin/release.json already rewritten. The CI checks this commit can break:
# the committed release.json (TestCommittedReleaseJSON; the other launcher
# tests write their own). The plugin package is checked by the workflow itself.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

go test ./tests/launcher/
