# Synthetic GGUF fixture provenance

These payloads were authored for Kama in 2026. They are project-owned test data
released under the repository's Apache-2.0 license.

Each decoded payload was assembled directly from the public GGUF v3 binary
layout: the `GGUF` magic bytes, a little-endian v3 header, and small synthetic
metadata key/value pairs. The malformed cases alter or truncate that same
project-owned structure. The Base64 files are a text-safe representation of the
decoded payloads named in `SHA256SUMS`.

The fixtures contain zero tensor descriptors and zero tensor data. They contain
no model weights, no prompts or responses from a model, and no third-party model content.
