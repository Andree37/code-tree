# code-tree

A dependency graph of the work in a repo, shared by you and your agents.

A goal becomes a **HEAD**. Anything that has to be settled first becomes a **thread** below it.
A node is **Waiting k/n** until its n children are resolved. The leaves are the frontier: they are
**Open** (an agent can take it), **Working**, **Needs you** (a question for a human) or **Ready**
(done, waiting for your review). `unblocks N` counts how many unresolved nodes above a leaf it
holds up, so the most useful work is easy to spot.

```
◆ [Waiting 0/1]  maps-and-sets-in-std  Maps and sets in Std
└─ [Waiting 0/3]  map-pattern-matching-in-match  Map pattern matching in match  · unblocks 1
   ├─ [Needs you]  recursive-case-vs-flat-binders-in-map  ...  · unblocks 2
   ├─ [Ready]      include-std-set-patterns-alongside  ...    · unblocks 2
   └─ [Open]       comptime-constant-vs-dynamic-pattern-keys  · unblocks 2
```

## Why plain files

Each node is `.tree/nodes/<id>.md` (frontmatter, `## Context`, `## Log`). A child stores the edge
(`blocks: [parent]`), so adding work never edits someone else's file and parallel agents rarely
conflict in git. Agents can read the graph in these ways:

| What | For |
|---|---|
| `.tree/GRAPH.md` | Regenerated on every change: a **Next** table, code-sync status, and a mermaid diagram and table per HEAD |
| `ct next --for agent` | The unblocked, unclaimed frontier, highest leverage first |
| `ct show <id>` | One node plus its chain up to the HEAD and the outcomes of its children |
| `ct ls --all` | Every tree, resolved nodes included |

Everything is plain text, including the node files, `.tree/sync` and the data the web page reads.

`.tree/PROTOCOL.md` (also `ct prompt`) tells agents how to behave: claim, split, ask, log and resolve.

## Install

```sh
go install github.com/Andree37/code-tree/cmd/ct@latest
# or from a checkout: go install ./cmd/ct
```

One static binary with no dependencies beyond the Go standard library. The web page and the agent
protocol are embedded (`cmd/ct/graph.html`, `cmd/ct/protocol.md`).

Optional: copy `skills/code-tree/` to `~/.claude/skills/` so Claude Code picks the protocol up
by itself, or add one line to a repo's `CLAUDE.md` / `AGENTS.md`:

> Work is tracked in `.tree/`. Read `.tree/PROTOCOL.md` and use the `ct` CLI.

## Use

```sh
ct init
ct add "Maps and sets in Std" -m discuss                   # HEAD
ct add "Map pattern matching" --blocks maps-and-sets       # thread under it (ids match by prefix)
ct add "Comptime vs dynamic keys" -b map-pattern -d "context for whoever picks it up"

ct ls                       # tree view (--all includes resolved)
ct next --for agent         # what an agent should take   (--for human: needs-you + ready)
ct show map-pattern         # why it matters, what is decided below, full log

ct claim comptime --by claude
ct note  comptime "tried X, Y breaks because ..."
ct ask   comptime "Allow runtime keys? I recommend no: ..."      # -> Needs you
ct ready comptime --outcome "keys must be comptime; runtime keys use Std.Map.get"
ct done  comptime           # you accept it; the parent moves to Waiting 1/n

ct link A B / ct unlink A B # A blocks B (cycles are refused)
ct drop ID / ct rm ID / ct edit ID / ct set ID <status>

ct view                     # static .tree/graph.html
ct serve                    # same page, live-reloads as agents change the tree
```

The page has three tabs: **Next** (needs you, ready for review, open for agents), **Graph** (one HEAD
at a time, like the tree above) and **Tracker** (a board with one column per state).

The author on log lines comes from `--by`, then `$CT_AGENT`, then `human`. Give each agent its own
`CT_AGENT` so the log shows who did what.

## Keeping up with the code

The tree never touches your code repo: no hooks, no commit trailers, no files in it. It only reads git
(`rev-parse`, `diff`, `merge-base`), and only when you run `ct sync`.

```sh
ct init --code ../my-zig-project     # keep the tree in its own folder/repo, pointing at the code
CT_TREE=~/plans/my-zig-project ct next   # or point at a tree from anywhere

ct add "Map impl" -b maps -t src/std/map.zig     # nodes say which code they own: paths, dir/, globs
ct done map-impl --touches auto                  # or: everything changed since the node was claimed
ct touch map-impl src/std/map/                   # add ownership later (--rm to remove)

ct sync            # diff code since the last sync; rewrites only the nodes whose files changed
ct sync --check    # same, but exit 1 if anything is stale or unmapped (for CI)
ct ack map-impl --note "append-only change, outcome holds"
```

Each sync makes one `git diff --name-status` from the last synced commit (kept in `.tree/sync`)
to HEAD and looks only at the files that changed, so it stays fast on a big codebase and is instant when nothing changed.

- A **resolved node whose files changed** becomes **Stale** (red, dashed) and shows up in Next.
  Recheck it: `ct ack` if the outcome still holds, `ct open` if it doesn't.
- **Renamed files** update the node's `touches` automatically.
- **Changes no node owns** are listed as **unmapped** in `.tree/GRAPH.md` until a node claims them.
  Noise like lockfiles is filtered by `ignore =` in `.tree/config`.

Ownership works on paths, so any language works the same way.

## Rules the CLI enforces

- A node with unresolved children can't be marked `ready` or `done` without `--force`.
- `claim` refuses a node someone else is working on, or one that is still waiting.
- Adding a child to a `ready` or `done` node reopens it.
