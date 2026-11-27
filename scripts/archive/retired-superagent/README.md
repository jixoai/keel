# Retired super-agent acceptance scripts

The super-agent ticket board was deleted in `keel-board-v1` Phase 2. These
scripts called its HTTP surface (`GET /v1/superagent/tickets`,
`/v1/superagent/attention`, operator answer/resume/stop), which is now 404.
They are kept only as history and must not be run against the current server.

The board is the one task face: a live end-to-end check now needs a new board
lane (`/v1/board/me`, `/v1/board/tasks`, `/v1/board/tasks/{id}/...` and the
superagent command bridge), not this directory.
