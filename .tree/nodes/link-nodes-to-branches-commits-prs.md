---
id: link-nodes-to-branches-commits-prs
title: Link nodes to branches, commits, PRs
mode: discuss
agent: 
status: dropped
blocks: code-tree-v0-2
outcome: 
created: 2026-11-22 14:04:19
updated: 2026-11-22 14:40:15
at: 7594fd38fd8695ba367dd6bcdbf043f836c19271
---

## Context
Optional refs: field per node; ct could show git log for a node's branch. Decide: free-form refs or git trailers (Tree-Node: <id>)?

## Log
- 2026-11-22 14:04:19 [human] created
- 2026-11-22 14:04:20 [claude] open → needs-you: QUESTION: Free-form refs field, or git commit trailers (Tree-Node: <id>) so the link lives in history? I lean trailers.
- 2026-11-22 14:40:15 [human] needs-you → dropped: moot: the tree never writes to the code repo; ct sync follows paths instead
