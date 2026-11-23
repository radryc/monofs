# MonoFS Pipelines

The authoritative pipeline guide is
[internal/router/help_pipelines.md](../../internal/router/help_pipelines.md).

Pipeline configs are registered and loaded from the Guardian KVS prefix
`/.pipelines/`; the router does not scan a workspace checkout or load local
files. The YAML in this directory is the authoring convention and example set,
not an auto-discovered source. `source_dir` is not a user field — scope with
`on.push.paths`/`paths-ignore`.

Ready-to-copy examples live in [`examples/`](examples/).
