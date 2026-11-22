---
name: code-tree
description: Work from the project's code-tree dependency graph in .tree/. Use when a repo has a .tree/ directory, when the user asks what to work on next, to break a goal into sub-problems, to record a decision or open question, or when the user mentions ct, code-tree, threads, heads, or "needs you".
---

# code-tree

This project tracks work as a graph of small nodes in `.tree/nodes/*.md`, driven by the `ct` CLI.

1. Run `ct prompt` (or read `.tree/PROTOCOL.md`) and follow it.
2. Orient with `.tree/GRAPH.md` or `ct next --for agent`.
3. Before working on a node run `ct show <id>`: it gives the chain up to the HEAD and the outcomes already decided below it.
4. Claim with `ct claim <id> --by claude`, split prerequisites with `ct add "<title>" --blocks <id>`, escalate with `ct ask`, finish with `ct ready <id> --outcome "..."`.

If `.tree/` does not exist and the user wants to start one, run `ct init` and turn their goal into a HEAD with `ct add "<goal>" -m discuss`.
