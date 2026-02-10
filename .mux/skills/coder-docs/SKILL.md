---
name: coder-docs
description: Index + offline snapshot of coder/coder documentation (progressive disclosure).
---

# Coder Docs

This skill bundles a text-only snapshot of the documentation from
[`coder/coder`](https://github.com/coder/coder).

## How to use

- Use the generated **Docs tree** below to locate a topic, then read the exact page:

  ```ts
  agent_skill_read_file({
    name: "coder-docs",
    filePath: "references/docs/install/docker.md",
  });
  ```

- To see the full navigation manifest:

  ```ts
  agent_skill_read_file({
    name: "coder-docs",
    filePath: "references/docs/manifest.json",
  });
  ```

- For keyword search, use ripgrep directly:

  ```bash
  rg -n "<keyword>" .mux/skills/coder-docs/references/docs
  ```

## Snapshot

<!-- BEGIN SNAPSHOT -->
<!-- END SNAPSHOT -->

## Docs tree (auto-generated)

<!-- BEGIN DOCS_TREE -->
<!-- END DOCS_TREE -->
