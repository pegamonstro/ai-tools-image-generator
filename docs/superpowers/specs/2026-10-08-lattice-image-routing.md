# Spec: All image inference through the lattice (routing + name/value split)

Date: 2026-10-08. Decision: generate and edit go through the lattice
frontend like every other inference call (chat, enhance); the direct
sidecar URLs remain the fallback transport and serve the modes the
lattice does not proxy yet.

## 1. Why

Before this change only prompt enhancement crossed the lattice;
generate/edit bypassed it with direct sidecar URLs. That gave img-gen its
own private routing layer — model names meant sidecar args, no gateway
queue management, no per-host model registry, and the lattice's
`/v1/images/models` listing did not describe what img-gen actually asked
for. The SDXL engine made this worse: a second engine with its own URL,
routed by img-gen's catalog only.

## 2. Routing decision

| op | transport |
| --- | --- |
| generate, edit | lattice frontend (`POST /v1/images/generations|edits`) — OpenAI Images shape: `model` = registry name, `size` = `"WxH"`, `n: 1`, `response_format: "b64_json"` |
| pose, inpaint (fill), outpaint/redux, upscale | direct sidecar URLs — the lattice does not proxy `/pose`, `/fill`, `/redux`, `/upscale` |
| progress, cancel, result recovery | direct sidecar — restart-hardened recovery keys off the sidecar's gen id and results store |
| chat, enhance | lattice frontend (unchanged) |

`IMAGE_ROUTING` selects: `lattice` (default) or `direct` (legacy sidecar
calls for generate/edit — the rollback). The lattice client fails loudly
when routing is `lattice` but the frontend URL is unset rather than
generating nowhere.

## 3. The name/value split

A submit carries a catalog **key** (`persephone`, `fluxedup`, `dev`,
`sdxl-base`, `sdxl-pony`, `sdxl-illustrious`) or a raw value. The queue
resolves once: the job keeps the resolved value (direct-sidecar fallback,
unchanged meaning in `/api/v1` responses and history) plus the submitted
key (`model_key`). The lattice path sends the key as `model`; a raw
_value_ without a key is sent as-is and only generates when the serving
host pins that exact name — otherwise the request fails at the lattice
with a clear provider error.

LoRA values and the sampling knobs (`seed`, `steps`, `guidance`,
`negative_prompt`) ride the request body verbatim as lattice extensions;
the gateway forwards unknown fields to the engine, so no registry is
involved for LoRAs. The lattice echoes `data[0].seed` — the seed the
engine's run actually used — which the job records as before.

## 4. Engine selection on the lattice path

The sdxl engine selection (`"sidecar": "sdxl"` in the catalog) stays a
submit-time derivation. On the lattice path the engine identity is
carried by the model's registry name: the serving lattice host announces
sdxl-pinned names as image models and routes them to its sdxl sidecar.
The direct-sidecar fallback resolves `SDXL_URL` from the job's `sidecar`
field as before.

## 5. Acceptance

- generate/edit with a catalog key route to the lattice by key; direct
  mode still hits `/generate`/`/edit` on `IMAGE_URL` (rollback tested).
- `IMAGE_ROUTING=direct` restores the legacy behavior end-to-end.
- LoRA + sampling extensions survive the proxy and land on the engine.
- seed recorded on the job equals the engine's used seed.