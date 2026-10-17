# Frozen knowledge publications for the formal round

Each directory holds one publication epoch of the formal round (docs/kb_experiment_design.md, 正式轮规程):

- `spec.json`: the kb-publish input. Its corpus path and sha256 name a file kept outside git (frozen by digest, mirrored on the NAS).
- `kb-publication.json`, `catalog-fragment.yaml`, `inputs/*.json`: what kb-publish produced for that spec on 2026-10-17
  (revision 2: chunk Result cell text_sha256). Publishing is deterministic: a fresh republish produced these files byte for byte
  and the same stored vectors (ledger KB1-LOOP-80). Every formal deployment republishes from the spec and must reproduce these
  files byte for byte, or the deployment fails.

The profile Catalogs under `../profiles/` are generated from these fragments.
