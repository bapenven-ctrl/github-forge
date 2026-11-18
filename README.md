# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/bapenven-ctrl/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/bapenven-ctrl/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single Python 3 process, standard library
only — no git binary, no packages, no external services. One `main.py` serves
four surfaces over one in-memory store:

- **Git smart HTTP** — clone and push, protocol v0 and v2 (`info/refs`,
  `git-upload-pack` with ls-refs, shallow/deepen fetches and sideband packs,
  `git-receive-pack` with per-ref report-status), a real SHA-1 object store
  with packfile read/write including delta resolution.
- **REST v3 API** — repositories, branches, commits, contents (base64),
  labels, issues and comments, pull requests with files and squash/merge,
  pagination and GitHub-shaped JSON throughout.
- **GraphQL v4** — the queries and mutations `gh` and the GitHub MCP server
  send: viewer, repository, issues and pull-request connections with cursor
  pagination, `node(id:)`, introspection, and create/close/merge mutations.
- **MCP** — streamable-HTTP tools (`create_repo`, `get_repo`, `create_issue`,
  `list_issues`, `create_pull_request`, `merge_pull_request`, …) over both the
  modern `server/discover` era and legacy `initialize`.

The process binds an ephemeral port, registers it with the trial environment
(`MAKEFUN_ENV_URL`), and serves until the environment says done. State is
deterministic: a logical clock, never wall time, stamps every object.

## Run it

```sh
python3 main.py   # binds an ephemeral port and registers it with $MAKEFUN_ENV_URL
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=bapenven-ctrl&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
