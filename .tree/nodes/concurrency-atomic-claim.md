---
id: concurrency-atomic-claim
title: "Concurrency: atomic claim"
mode: build
agent: 
status: open
blocks: ["code-tree-v0-2"]
outcome: ""
created: 2026-11-22 14:04:20
updated: 2026-11-22 14:04:20
---

## Context
Two agents can claim the same node at once. Use a lock file or O_EXCL write in .tree/locks/.

## Log
- 2026-11-22 14:04:20 [human] created
