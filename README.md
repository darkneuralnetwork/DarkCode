# DarkCode

> An Enterprise-Grade Modular Agentic OS in Go
<p align="center">
  <b>Architected and Built by <a href="https://darkneuralnetwork.com">Team Dark Neural Network (DNN)</a></b>
</p>
<p align="center">
  <img src="gui_nexus.png" alt="DarkCode GUI Interface" width="100%" />
</p>

## Overview

Traditional AI coding assistants inherently suffer from architectural bottlenecks, such as unbounded context windows resulting in extreme latency, exponential API costs, and degraded reasoning due to "lost-in-the-middle" attention deficits.

**DarkCode** reimagines autonomous AI engineering as a **Distributed Directed Acyclic Graph (DAG) of state-mutating micro-tasks**. By fundamentally separating high-level cognitive orchestration (planning, synthesis) from low-level execution operations (file I/O, compilation, diffing), DarkCode enables hybrid execution topographies. You can route deep-reasoning planning tasks to frontier cloud models, while offloading high-throughput token generation tasks (such as code editing) to local embedded quantization models.

## Architectural Differentiators

### 1. Intelligent Task Decomposition & Sub-Agent Dispatch
DarkCode diverges from unbounded ReAct loop agents by employing deterministic dependency-mapped task graphs (DAGs). In `Project` and `Loop` routing modes, complex user objectives are computationally decomposed into discrete, parallelizable nodes. Child nodes block on prerequisite completion, and sub-agents operate within narrowly constrained system prompts. 

### 2. Pure-Go Vector Retrieval & Memory Architecture
DarkCode operates a native, dependency-free memory subsystem comprising 4 tiers of persistence (Short-Term, Episodic, Semantic, and Knowledge Graph).
- **Semantic Vector Index**: Context is dynamically generated via embedded `llama.cpp` implementations. Search queries are resolved via a high-performance pure-Go Cosine Similarity engine without relying on external databases.
- **Context Compression Checkpoints**: Real-time token budget calculation continuously monitors the conversational history. When thresholds exceed configured limits (e.g., `ContextLength = 16000`), a lightweight model asynchronously summarizes and compacts older dialogue while preserving high-fidelity recent context.

### 3. Consensus Engine for Architectural Integrity
To mitigate hallucinations during critical system architecture design, DarkCode integrates a multi-model consensus synthesizer. It seamlessly fans out architectural inquiries to a heterogeneous matrix of models (e.g., GPT-4, Claude 3.5), performs cross-model polarity-aware conflict detection, and generates a unified response.

### 4. Smart Auto-Detection & Local Task Offloading
- **Smart Intent Classification**: Replaces traditional manual mode-switching with an LLM-driven classifier that automatically transitions between `general` (conversational), `project` (full workspace tool usage), and `loop` (Sense-Think-Act autonomy) based on inferred user intent.
- **Micro-Task Offloading**: Operations flagged as `TinyLocal` or `MediumLocal` (e.g., syntax reviews, error explanations) bypass the cloud LLM layer and route directly to local instances, significantly reducing network latency and API expenditure.

### 5. Enterprise-Grade Capability & Security Model
Agentic freedom necessitates deterministic constraints. DarkCode strictly scopes tool access using capability configurations. Actions possessing potential destructiveness—such as remote network calls or native shell executions—are actively gated via standard `safety_level` thresholds requiring explicit user authorization.

---

## Modes of Operation

- **GUI Mode (`--gui`)**: A responsive, locally-hosted web application delivering an IDE-level control plane. Features include a live workspace file-tree, telemetry visualization, visual DAG blueprints, and real-time model configuration panes.
- **CLI / TUI Mode (`--tui`)**: A lightweight, high-performance terminal UI leveraging BubbleTea. Ideal for CI/CD environments or terminal-bound developers, providing full parity with the GUI component.

<p align="center">
  <img src="tui_mode.png" alt="DarkCode TUI Mode" width="100%" />
</p>

## Quick Start Guide

### Prerequisites
- Go 1.22+

### Installation & Execution
```bash
# Clone the repository
git clone https://github.com/darkcode.git
cd darkcode

# Compile binary
go build -o darkcode .

# Execute with Web UI
./darkcode --gui
```

### Configuration
DarkCode persists configurations securely in `~/.darkcode/config.json`. Register models dynamically via the CLI:
```bash
darkcode --add-model claude-3-5-sonnet --provider anthropic --api-key sk-ant-...
```

For comprehensive technical specifications, detailed command references, and architectural deep-dives, please consult the [Official DarkCode Wiki](WIKI.md).
