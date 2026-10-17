# Explicit channels and complete-argument anti-truncation

This local fork adds model-name policies to **OpenAI Chat, OpenAI Responses and Gemini generateContent**. One provider/base URL is enough; no custom header is required.

## Model IDs

| ID | Policy |
|---|---|
| `<base-model>` | Legacy automatic channel selection, unchanged |
| `playground/<base-model>` | Playground only |
| `build/<base-model>` | Build only |
| `playground/anti-truncation/<base-model>` | Playground plus internal text transport |
| `build/anti-truncation/<base-model>` | Build plus internal text transport |

There is no `auto/` or channel-less `anti-truncation/` namespace. Reserved namespaces cannot be reused by `MODEL_MAPPING`. Prefix parsing precedes mapping, catalog aliases and thinking suffixes; quota and upstream requests use the canonical base model.

`GET /v1/models` and Gemini model get/list expose eligible variants. Variants are catalog candidates, **not proof of an end-to-end successful request**. Build variants do not inherit Playground continuation limits. A zero/missing channel limit means unknown. Embeddings, live, audio/video-specific endpoints, Interactions and Anthropic do not enable these policies; a reserved prefix there is rejected rather than ignored.

## Strict routing

The policy survives candidates, account selection, lease validation, protected sends, authentication replay, retries and continuation. Explicit Build never borrows Playground to recover from quota, permission or parameter errors. Same-channel account recovery remains possible before semantic output. Build's existing unary-to-stream fallback remains within Build.

Known incompatible tools, structured schemas, resource/speech requirements or output budgets fail explicitly. They are not made successful by removing requested tools, reducing the requested output budget or changing channels. Build uses its native catalog output limit; a Playground continuation estimate is not itself a Build capability.

Authentication, proxy, worker/WAA and account concurrency remain shared. Channel-local cooldowns remain separate; a model or auxiliary-request success cannot clear a still-active account-wide quota limit.

## What anti-truncation does—and does not do

A stable internal function carries the complete final text in `content`. The core validates the full argument object and projects it into ordinary text before protocol adaptation. Real client tools retain their IDs, arguments and signatures. A required real tool cannot be satisfied by this internal transport.

- Ordinary text is bounded and buffered until the source is selected; valid transport text takes precedence, preventing duplicate answers.
- Broken, repeated or conflicting transport calls fail. There is no JSON repair or automatic model call to complete them.
- Stop sequences apply to projected text. Length, safety, upstream errors, cancellation and missing terminal events are not rewritten as success.
- Raw upstream usage remains distinct from local estimates; it includes internal transport overhead. Auxiliary input counting describes its submitted counting request and is not a substitute for observed generation usage.
- Native Argon continuation remains independent. There is no new cross-round repair of incomplete function arguments.
- **This is not incremental function-argument streaming.** SSE remains available, but transported text can arrive only when the complete arguments are available. No fake timed chunking is added.
- Transport success alone does not prove that premature termination is less frequent. That needs a separately budgeted real comparison.

## History and Responses

All three protocols keep real tool-call → tool-result → final-answer flows. Multiple Chat choices and Gemini candidates retain independent request IDs and policy. Responses creation, background streams, history and compaction-generated requests use the same generation boundary; retrieving or cancelling a saved response does not generate again.

Explicit policies only restore signatures with known matching model/channel provenance. Native signatures without a call ID are also attributed. Unknown or incompatible opaque state is rejected, not silently deleted. Known strict-origin state cannot escape through unpinned `auto`; legacy bare-to-bare histories retain compatibility. Plain portable history is not artificially channel-bound. Provenance is bounded and in-memory: after restart/eviction an opaque history may require an explicit failure rather than guessed restoration.

Structured-output contracts are not silently converted to plain text for anti-truncation. Unsupported combinations—including a compaction generation that cannot retain its requested schema—fail explicitly.

## Observability and storage

The existing ledger adds requested/base model, requested/actual channel, strategy, stable root/choice IDs and physical-attempt snapshots. A new send gets a new attempt ID; revisions update the same attempt. Preparation failures, entering the send boundary and receiving a response are different facts. Entering the send boundary does not prove Google executed or billed a request.

Old outer-failure entries remain labelled legacy/unknown. Logical usage, per-attempt usage and estimated/mixed/unattributed sources are not conflated. Attempts are not added again to logical token totals.

New nullable metadata lives in `request_metadata` in the same SQLite database. The original 20-column `requests` table remains compatible with the old writer. New request and metadata writes are transactional; old-writer rows remain unknown, and restoring the new reader does not require overwriting newer data with an old backup. This is an operational ledger, not lossless billing: the original asynchronous queue, failure and retention limitations remain.

## Validation boundary

Offline tests use invented accounts, fake protected transports, HTTP recorders and temporary databases. They cover protocol contracts and failure boundaries, not live Google rights, Build quotas, login renewal in production, long-output effectiveness or latency under load. Deployment, client configuration changes and real requests are separate operations.
