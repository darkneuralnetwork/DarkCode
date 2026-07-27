---
title: DarkCode
---

# DarkCode

A local-first coding agent that understands your repository structurally —
what defines what, what a change will break, and what it does not know.

Everything runs on your machine. There is no server component, no telemetry,
and the whole thing is a single static binary with four third-party
dependencies.

## Start here

- **[README](https://github.com/darkneuralnetwork/darkcode#readme)** — what it
  is, how it works, and how to install it.
- **[Quick start](https://github.com/darkneuralnetwork/darkcode#-quick-start)** —
  from clone to first answer.

## Guides

| Page | What it covers |
| :-- | :-- |
| [Threat model](THREAT_MODEL.md) | What DarkCode defends against, what it does not, and how to verify a release |
| [Benchmarks](BENCHMARK.md) | Running the suite, and why a quota error invalidates a score rather than lowering it |
| [Contributing](https://github.com/darkneuralnetwork/darkcode/blob/main/CONTRIBUTING.md) | Repository history, dependency policy, and the measurements behind it |

## What makes it different

**It reads structure, not just text.** A persistent knowledge graph records
which files define which symbols, what imports what, and how confident it is in
each fact — so "what will this break?" is a query rather than a guess.

**It can undo itself.** Every mutating tool call is checkpointed, and a
rollback rewinds the conversation alongside the filesystem, so the agent does
not carry on reasoning from state that no longer exists.

**It proves changes before keeping them.** Competing fixes are each applied,
run against the project's own verifier, and rolled back; the one that passes
wins, and if none passes that is reported as "keep none" rather than as a
winner.

**It stays quiet.** The health daemon watches structure in the background under
a hard share of one core, so a cycle appearing is noticed when it appears
rather than when somebody remembers to look.
