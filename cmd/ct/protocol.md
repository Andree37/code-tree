# code-tree protocol

This project tracks its work as a dependency graph in `.tree/`. Read this before
touching code, and keep the graph honest while you work.

## Model
- Each node is `.tree/nodes/<id>.md`: frontmatter + `## Context` + `## Log`.
- A node *blocks* its parents. A parent is **Waiting k/n** until all n children
  are done or dropped. Nodes with no parent are **HEAD**s (top-level goals).
- Stored status: `open` · `working` · `needs-you` · `ready` · `done` · `dropped`.
- `unblocks N` = how many unresolved nodes above this one it is holding up.
  Higher means more leverage.

## Reading the graph
- Overview: `.tree/GRAPH.md` (regenerated automatically, never edit it).
- Everything is plain text: `ct ls` for the tree, `ct show <id>` for one node with its full context.
- `ct show <id>` prints the chain up to the HEAD (why this matters) and the
  outcomes of resolved children (what was already decided below you). Read it
  before you start.

## Working loop
1. `ct sync`, then `ct next --for agent` lists unblocked, unclaimed work by leverage.
2. `ct claim <id> --by <your-name>` marks it working so nobody else takes it.
3. Found a prerequisite that must be settled first?
   `ct add "<title>" --blocks <id> -d "<context>"`. The parent becomes Waiting.
   Nodes you add start as **Proposed**: a human accepts, merges or rejects them.
   You cannot claim a proposed node, and you must never `ct accept` your own.
   Keep nodes small: one question or one change each, and propose few.
4. Need a human decision? `ct ask <id> "<question with options and your recommendation>"`.
   The node becomes **Needs you**. Stop working on it and pick other work.
5. Log progress: `ct note <id> "<what you did / learned>"`.
6. Finished: `ct ready <id> --outcome "<one-line result>"` so a human reviews it,
   or `ct done <id> --outcome "..."` when the user said no review is needed.
   The outcome is what parents and other agents will see, so make it self-contained.

## Code
- The tree describes the code repo named in `.tree/config` (`code = ...`). It only
  reads that repo; never add hooks, trailers or tree files to it.
- Say which code a node owns: `--touches src/std/map.zig` (or `dir/`, globs) on
  `add` / `ready` / `done`, or `ct touch <id> <paths>`. `--touches auto` records
  everything changed since you claimed the node.
- Run `ct sync` before `ct next`. It diffs the code since the last sync and marks
  resolved nodes whose files changed as **Stale**. Recheck a stale node: if its
  outcome still holds, `ct ack <id> --note "why"`; if not, `ct open <id>` and fix it.
- Changes no node owns are listed as unmapped in `.tree/GRAPH.md`. Attach them to the
  right node with `ct touch`, or add a node for them.

## Decisions and checks
- `ct show <id>` lists **Decisions in force**: answers and rulings from this node and
  every node above it. They are constraints. Follow them; if one blocks you, `ct ask`.
- A node may have a `check` (a shell command run in the code repo). `ct ready` / `ct done`
  run it and refuse when it fails. Never use `--skip-check` unless the human told you to.
- Discuss nodes close with the decision as the outcome: `ct done <id> --outcome "..."`.

## Talking (intercom)
- ct talks through intercom (`ic`). When the human answers, accepts, decides or sends a
  node back, the agents on that node get a message automatically, at a safe moment.
- `ct talk <id> "..."` messages the agent working on a node; `ic send <name> "..."` and
  `ic ask <name> "..."` reach any session (`ic who` lists them). Answer with `ic reply`.
- If `ct claim` warns that another agent touches the same files, agree with it over
  `ct talk` before editing them.

## Rules
- Never mark something done that you did not verify.
- Do not resolve another agent's `working` node.
- Prefer adding a child over growing a node's scope.
