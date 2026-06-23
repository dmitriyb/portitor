# spec/

Design-of-record for portitor, written in the **spexmachina spec format** so it can later be run
through the `spex` / `/spec` workflow.

- `proposals/` — hand-authored change proposals (the workflow's entry point). Start here.
- `<module>/module.json` — module metadata: requirements + components. **Seed only**: the identity
  hashes (`id`, `preq_id`, `spec_node_id`) and `spec/.snapshot.json` are *generated* by `spex`'s
  identity layer when you run the spec workflow — they are intentionally omitted from these
  hand-written seeds.
- `<module>/{arch,impl,test,flow}_*.md` — the content files (architecture, implementation,
  test scenarios, data flows) referenced by each component's `content` field.

To formalize: run the proposal + these seeds through your spex/`/spec` workflow to generate the
identity hashes, the formal `module.json`, and `spec/.snapshot.json`.
