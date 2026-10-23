# Assistant route — text candidate with upstream-style tool downgrade

This branch adds the Build **code assistant** as a third explicit text route. It does not replace the Build application proxy, update the deployed instance or prove live account/model availability. The accepted scope is text-only; client-native tools and built-in-tool anti-truncation investigations have ended.

## Route and catalog

- Branch: `experiment/assistant-argon-probe`, based on `6761bde`; work remains uncommitted.
- Model ID: `assistant/<base-model>`, for example `assistant/gemini-4-argon`.
- Explicitly enable the `assistant` upstream channel; defaults stay `playground,build`. Settings take effect on generation-service restart, which has not been performed by this development task.
- Bare-model auto never selects Assistant. Explicit PG/Build requests cannot fall back to Assistant, or vice versa.
- No upstream `<model>-build` compatibility alias. `assistant/anti-truncation/<model>` is neither advertised nor executable: it is rejected, never converted to a plain request.
- Eligibility uses account catalog capability code 48, not model-name assumptions. Eligibility is not a live rights/quota check. Native token bounds remain unknown (zero metadata); PG continuation is not borrowed.
- Catalog does not advertise function declarations, Google tools, continuation or anti-truncation for Assistant.

## Intentional tool downgrade

The user has chosen the upstream AIStudio2API **text adaptation**, not native harness compatibility. For Assistant only:

1. Function declarations, known Google tool declarations and GoogleSearch options are removed.
2. Tool-choice mode, allowed function names and parallel-tool controls are cleared, including `required`, named choices and `parallel_tool_calls=false`. `none` is normalized to no tools too. Unknown Google tool names remain invalid arguments, including under `none`.
3. Historical function calls and results become JSON **text** using the existing transcript format. A result without a name is linked by historical call ID or the existing unique pending-call rule; ambiguous unnamed results fail before resources. Call IDs are used for linking, but the upstream transcript format contains name/arguments/result, not native call IDs or signatures.
4. Function signatures/opaque tool state are not sent as part of the transcript. API signature provenance checks still happen before core downgrade, so a foreign/unknown signed history is not legalized by text conversion.
5. No client tool executor, function-call emulation or output text parser is added. A normal text answer does not satisfy the client's original `required` tool contract. Clients relying on actual tool use must not select this route.

These rules run before pooled validation/resource selection, client strict capability checks, direct send and public encoding. They are idempotent and copy content/part slices, leaving the original request unchanged. PG/Build/auto retain their previous strict/native-tool behavior. Normal text/thought Parts are not rewritten. Mixed function+media/code Parts are invalid instead of being reduced to text and thereby bypassing limits.

This is a selective port of upstream `a784c5193b3ba5c4894d536b2f4cd73e87f99475`, `tool_validation.go` and `generate.go`. Two deliberate normalizations are documented: clearing Tools even under `none`, and rejecting unknown Google tool names even under `none`. No blanket upstream upgrade is performed. Source/evidence for the earlier native-tool investigation is retained in [assistant-tool-wire-evidence.md](assistant-tool-wire-evidence.md) and [assistant-editor-module-evidence.md](assistant-editor-module-evidence.md); those documents are historical investigations, not pending feature commitments.

## Text features and remaining limits

OpenAI Chat, OpenAI Responses and Gemini generateContent/streamGenerateContent adapters map text output, thought output, system instructions, normal conversation and thinking strength. Responses background/history/compaction generation stays pinned to Assistant; retrieving/cancelling saved Responses objects does not create another generation. Output, known usage and terminal errors use existing adapters without fabricated `tool_calls`.

Still rejected rather than ignored: attachments/uploads/Drive references/external media; explicit `max_tokens` / `max_output_tokens`, temperature/topP/topK/seed; enforced output schema/JSON MIME; non-text modalities, image/speech/transcription configuration, media resolution, per-category safety settings; executable-code/result history. Clearing tools does not make these fields supported. Clients that always send output budgets or other unsupported controls must omit them for this route. A minimal prompt is not an enforceable server output budget.

Stop matching and hiding thoughts are local projections. Unexpected native function/media output Parts still produce protocol-evidence errors, not empty text success. The known protocol reports step completion, not a complete Gemini safety/length taxonomy; unknown details remain unknown.

## Side effects, state and retries

Each request creates one temporary Applet, submits a WAA-protected `CodeAssistantOffline` task, reads that task using `StreamCodeAssistantOfflineGeneration`, cancels if unfinished, then deletes only the new Applet. Stream reading/control calls are not another generation attempt. Assistant-local cooldowns/stats are separate from Build; account-wide limits, WAA, proxies and total concurrency remain shared. Separate route namespaces are not a guarantee of independent Google allowances.

The non-idempotent send boundary blocks outer no-text replay even if headers are missing. An accepted submit with no readable task ID or failed stream open is attributed to that submit, not resubmitted for success. Existing bounded authentication recovery may create separately recorded physical sends; inspect the ledger instead of assuming one client request always means one send.

Cleanup is once-only, synchronous and bounded to one minute with the same account lease; failed cleanup is logged and can leave resources. A successful create with an unreadable Applet ID can also leave an unidentified resource. Tool downgrade changes only client tools: it does not provide a server-side denylist for Assistant's own tools. Static investigation has not established a way to guarantee no compile/execute/network activity inside the temporary Applet. This candidate does not implement built-in-tool extraction, file answer retrieval or anti-truncation, and the Agent has not tested live task side effects.

## User smoke test — after a separately approved deployment

The prior text-downgrade build was deployed on 2026-10-19. The 2026-10-20 completion fix below has **not** been executed or deployed, and no service restart, configuration/Pi change or real model request was made for it. Do not share account state between simultaneous instances (WAA cross-process locks). Existing Start/Stop-Release launchers remain bound to the previously approved hashes.

Once deployment is separately approved and Assistant explicitly enabled, choose an ID listed by that instance. A minimal Chat payload is:

```json
{
  "model": "assistant/gemini-4-argon",
  "messages": [{"role": "user", "content": "Reply briefly with ok."}],
  "stream": true
}
```

It is only an example, not an Agent-executed request. Tool declarations/history are now accepted for the documented **downgrade**, not passed through as native tools. Leave unsupported token/sampling/schema controls absent. A 403/429 is a rights/quota blocker; a successful Assistant response neither validates tools/anti nor fixes the older Build Argon 429. Check actual channel, attempts, usage provenance and cleanup.

## Completion fix — 2026-10-20, not deployed

After a closed root JSON array and successful existing step/trailer validation, the decoder now calls `End()` without an extra read for HTTP EOF. The server may keep the transport open after delivering the complete RPC envelope. This removes only that post-root wait: an unclosed root, incomplete steps, failure trailers and unknown statuses remain errors, not synthetic success. No idle-timeout success or early completion on a model step alone is introduced. Bytes beyond the closed root are outside this decoder's completion boundary and are no longer probed with a blocking read.

Cleanup still runs synchronously under the same account lease; slow cleanup can therefore still delay source closure and Responses termination. This change does not redesign cleanup, relax step states or change any PG/Build decoder. The local three-protocol tests use blocked-at-EOF synthetic bodies and verify HTTP handler return and terminal events, not Google end-to-end behavior.

## Offline evidence boundary

Fixtures exercise pure downgrade (including input immutability, idempotence and concurrent requests), pooled validation and real client wire encoding, three protocols in both response modes, Responses background/history/compaction, strict anti/unsupported-field refusal, native-output errors, shared capacity, lease/proof seams and cleanup/terminal/usage behavior. Every transport byte is synthetic. These tests prove implementation behavior, not Google acceptance, long-term stability, reduced truncation or live lifecycle safety. Real canary remains user-owned.
