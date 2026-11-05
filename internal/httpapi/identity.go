package httpapi

import (
	"os"

	"github.com/google/uuid"
)

// pid and bootID identify this process instance. They are the evidence that a
// restart really happened in the stateless tests; nothing depends on them for
// correctness.
var (
	pid    = os.Getpid()
	bootID = uuid.NewString()
)
