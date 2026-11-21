package servicehost

// Test-only tool authorization seam. Production Boot requires the access
// bridge (cmd/keel wires toolauth); these tests drive the worker with a
// deterministic fixture that has no identity rows, so they explicitly opt out
// with the allow-all fake. The runner's default is fail-closed (A4.2b1).

import "github.com/jixoai/keel/internal/agent"

func allowAllToolAccess() agent.ToolAccess { return agent.AllowAllToolAccess{} }
