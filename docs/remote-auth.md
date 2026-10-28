# Remote Windows authentication helper (experimental, default off)

This capability moves authentication recovery, not generation, to the original Windows user/device. It is an independent executable and never calls `app.Run`, opens the generation/admin API, or starts WAA workers. Linux owns a single generation projection; Windows retains OAuth/device material and the precise Chrome source. No production setup or actual cross-egress authentication is implied by the offline candidate.

## Native entrypoints

`cmd/aistudio-auth-helper` defaults to plan-only. Every operation that reads configuration, state or credentials, connects to anything, or changes state requires **both** `-execute -confirm-account`. `-operation plan` never reads configuration/auth. Do not run the following against real accounts without a separately approved window.

- `serve -config <absolute Windows server config>`: loopback mTLS service. Default-disabled config is rejected. Run as the original Windows user; no SYSTEM substitution.
- `register -config ... -handle ...`: confirm a fixed account/source and persist a **disabled** authority. The original account can no longer acquire generation leases after registration, including while disabled.
- `enable|disable -config ... -handle ... -expected-epoch ...`: locally change policy/epoch. Exact source fallback additionally requires `-allow-source-recovery`; it is off by default.
- `rebind-source -config ... -handle ... -expected-epoch ... -chrome-root ... -profile ...`: the mapping must be disabled; original SID/Gaia are checked. Outstanding operations require `-acknowledge-prior-operations`. Imports only the selected source, preserves account configuration, and leaves the new epoch disabled. Never scans a default/other root.
- `return-local -config ... -handle ... -expected-epoch ... -confirm-old-consumer-stopped`: after the Linux master is actually drained/stopped and the mapping disabled, archive only nonsecret authority evidence and release local generation ownership. Unknown/pending operations reject this transition. Latest cookies/material remain in place; re-registration uses a later epoch. This does **not** revoke Google cookies already issued to the old consumer.
- `bootstrap -config <Linux client config> -auth-root ... -handle ... -gaia-id ... -email ... -expected-epoch ... -base-cookie ... -base-material ...`: create one disabled remote projection, obtain existing cookies without OAuth, validate, commit, acknowledge. It does not enable accounts or import Windows OAuth/Chrome root.
- `reenroll -config ... -state ... -handle ... -gaia-id ... -email ... -expected-epoch ... -base-cookie ... -base-material ...`: require a strictly later authorized binding; explicit `-acknowledge-prior-operations` is needed if replacing unsettled state.
- `reconcile -config ... -state ...`: resume incomplete bootstrap/pending or acknowledge committed state under an account lease. Not a generic model replay.
- `reconcile ... -keep-current-cookies`: explicit operator disposition for genuine CAS conflict after local cookie maintenance. Lookup-only obtains an existing helper result (never starts authentication), preserves current valid local cookies, commits control versions and acknowledges `keep_current`. Unknown helper work is not discarded.
- `inspect -state ...`: safe version/binding/operation metadata only; never prints cookies/material.
- `health -config <client config>`: authenticated no-material/no-Google readiness; not account/Google health.
- `signal-stop -target-pid ...`: same-Windows-user local stop event; no remote shutdown RPC. The Owner launcher checks exact path/hash/start time first and never forcibly kills a busy process.

The application opts into a native client config via `REMOTE_AUTH_CONFIG=<absolute path>`. Leave it empty for existing local behavior. Config path/key changes require restarting the generation service in an approved idle window. There is deliberately no remote credential-management UI.

## Protocol and persistence

Only `GET /healthz`, `POST /v1/refresh`, and `POST /v1/ack`; verified mTLS consumer identity and a fixed handle mapping are mandatory. Loopback HTTPS, no ambient proxy, no redirects, strict JSON/version/size/path handling. Client cannot select a Google URL, file path, proxy, Chrome root, command or account discovery target.

The authority durably writes an operation before authenticating, then atomically commits current cookies, necessary material changes and result metadata in the same state file. No separate secret response cache. Current and historical operation IDs are checked against immutable work parameters. Duplicate requests retrieve the saved result/failure; superseded history cannot execute again.

Windows keeps the complete native multi-domain cookie set, OAuth/device material, exact source, origins and existing root extensions. Registration, refresh eligibility, the common result/retrieval outlet and explicit authority source rebind use one private in-memory cookie projection: select by the fixed local mapping's cookie domains (default `google.com`), preserve order and all fields in a separate slice, then apply the unchanged strict cookie validator. In-domain invalid entries are not cleaned up; RPC limits apply to the selected subset, not the complete authority. Native import/renewal, ordinary local accounts and `return-local` retain complete cookies. Linux still rejects any out-of-policy cookie received over RPC, including keep-current reconciliation.

Protocol remains 1. `mapping_epoch / cookie_revision / material_version` identify the same authority generation and its deterministic subset, not byte-identical cookie arrays. Each side advances its own file CAS revision independently. Projection does not authenticate, write, advance a version or settle an unknown operation. Same-ID retrieval and ordinary ACK retain their existing rules; changing to an older helper can again reject an already-saved multi-domain authority even though the schema is unchanged.

The request has a bounded deadline and an optional `lookup_only` transport control. Lookup does not alter authentication work parameters or start OAuth. A request never accepted before its deadline can be retired only with a matching mTLS `NOT_STARTED` receipt, durably recorded by Windows. Linux persists that proof and stops that logical attempt; the next recovery can create a fresh operation. Client timeouts alone never authorize this transition. Persisted unknown operations remain unknown, including across process death.

All registered-session writers use a file CAS revision. Ordinary no-op writes do not invalidate pending state. Actual cookie maintenance increments the revision; automatic recovery then rejects stale results before runtime reset. The explicit native keep-current disposition preserves those newer local cookies instead of silently overwriting them. New-source/remote metadata must not be processed by older typed writers.

Windows single-flight uses the account/file lease across processes; a CNG deadline is not proof that native work or its lease ended. Stop closes admission and drains handlers without force; a stuck native call may leave the process running, not a false stopped/succeeded receipt. Same-directory locks do not protect copied directories or multiple generation masters.

One logical generation shares a single new-authentication budget across worker and RPC recovery. Safe remote failures outrank old 401/network/WorkerFailed causes, so helper outage cannot drive model replay, all-account failover or generation-proxy rebinding. Definite RPC authentication refusal may use the original bounded current-RPC replay, retaining channel/body/continuation; unknown sends and delivered output are not replayed.

`operation.http.sent/responses` count fixed-stage authentication HTTP metadata after a completed workflow. Nil means unknown/not captured, not zero; persisted pending after process death cannot imply that Google saw no request. No URL, body, headers, cookie, token or key are in these counters.

## Owner control material and delivery

Owner: `D:/AIStudio2API`. `scripts/Manage-AuthHelper.ps1` prepares guarded plan/start/inspect/stop/probe; templates under `scripts/templates/` are disabled/placeholder-only. SSH uses a **new dedicated** Windows-initiated reverse forward plus mTLS. Do not restore retired DMIT tunnels or reuse VMISS transport. Ports, SSH/TLS identities, actual config and account scope are assigned only in the real-machine gate. If Linux generation is containerized, separately verify its host-loopback access; never expose this helper publicly to solve a container routing problem.

The joint candidate includes the dedicated Chrome-root adaptation and this remote capability. Offline Go/frontend tests and Windows/Linux compile-only artifacts are not SSH, original-user CNG, Chrome import, cross-egress Cookie, model, migration or deployment acceptance. Those require the Owner execution's D/E authorization. Do not independently deploy the earlier Chrome-only candidate.
