# ADR-0014 — Plugin attachment contract: binary-blob delivery from plugins to vault

**Status:** Proposed (2026-05-06)
**Date:** 2026-05-06
**Depends on:** [ADR-0005](./0005-plugin-lifecycle.md), [ADR-0006](./0006-plugin-discovery-config-allowlist.md), [ADR-0008](./0008-vault-as-source-of-truth.md), [ADR-0012](./0012-user-generated-content.md)
**Tracked in:**

## Context

Plugins are subprocesses (per ADR-0005, ADR-0006) that communicate with the daemon via stdin/stdout JSON. They produce entity data which the daemon writes to the vault (per ADR-0008: vault-as-source-of-truth, daemon owns writes).

Some plugins emit binary content alongside the structured data:

- `yaad-bgg` will emit a thumbnail image per boardgame (BGG returns a `thumbnail` URL on every game record).
- Future plugins are likely to emit PDFs, audio, screenshots, or other binary blobs.

The current contract has no shape for binary delivery. Three plausible mechanisms exist, each with real trade-offs:

1. **Inline base64 in the JSON FetchResult.** The plugin embeds the bytes directly in the JSON response.
2. **Tmp file + URI reference.** The plugin downloads the binary to a tmp file, references the path in JSON; the daemon stages it into the vault.
3. **URL pass-through.** The plugin emits the upstream URL; the daemon fetches it and stores the result.

Each fits a different plugin shape:

- **base64** is simplest for tiny payloads or sandboxed plugins without filesystem access.
- **tmp+URI** is the natural fit when the plugin already needs to authenticate / handle upstream-specific quirks (BGG, GitHub, Wikipedia).
- **URL pass-through** is simplest when the upstream URL is canonical and stable, and the plugin doesn't add value over the daemon's HTTP client.

A canonical-single-method contract would be simpler to maintain but rules out plugin contexts the others handle naturally. Operator polling the discussion (see): all three should be supported within one wire shape, with the plugin choosing per-attachment.

## Decision

**Plugins emit binary data via a top-level `attachments[]` field in FetchResult. Each attachment is a `{role, uri, extension}` triple with a URI that uses one of three schemes (`file://`, `https://`, `base64://`). The daemon dispatches on scheme and places the resolved binary at `<vault>/<kind>/<id>.<role>.<extension>` next to the entity's `.md` file.**

### 1. Wire shape

FetchResult JSON gains a top-level `attachments` array:

```json
{
 "id": "boardgame:130680",
 "kind": "boardgame",
 "title": "Brass: Birmingham",
 "attachments": [
 {
 "role": "thumb",
 "uri": "file:///tmp/yaad-bgg-130680-thumb.jpg",
 "extension": "jpg"
 },
 {
 "role": "rules",
 "uri": "https://example.com/brass-rules.pdf",
 "extension": "pdf"
 }
 ],
 "...": "..."
}
```

Field semantics:

- **`role`** is a plugin-defined semantic identifier. Becomes part of the vault filename. MUST match the regex `^[a-z0-9][a-z0-9-]{0,31}$` (lowercase alphanumeric + dash, 1-32 chars, must start with alphanumeric). Examples: `thumb`, `cover`, `rules`, `screenshot-01`. Reused across re-ingests (a `thumb` re-emission overwrites the previous `thumb`). The daemon validates per §5 before any filesystem write.
- **`uri`** is the source. Required. One of three schemes (below).
- **`extension`** is the on-disk file extension WITHOUT the leading dot, lowercase. Required (no inference from URI). MUST match the regex `^[a-z0-9]{1,10}$` (lowercase alphanumeric, 1-10 chars). Examples: `jpg`, `png`, `pdf`, `mp3`. Daemon does not validate the extension matches the actual content type (that's the plugin's responsibility) but does validate the shape per §5.

A single attachment may appear at most once per `(role)` per entity per FetchResult. A plugin emitting two attachments with the same role on a single fetch is a plugin bug; the daemon SHOULD log + use the last one.

### 2. URI schemes

#### `file://<absolute-path>`

The plugin has staged the binary at `<absolute-path>` on the local filesystem. The daemon copies (or hardlinks if same fs) to the vault location, then deletes the source.

Constraints:

- Path MUST be absolute.
- Path MUST resolve under the operator-configured plugin staging directory (default `/tmp`, configurable via `config.plugin_staging_dir`). Paths outside the staging dir are rejected with a path-traversal error; the attachment is logged + skipped, the rest of the entity proceeds.
- Daemon deletes the staged file after a successful copy. If the copy fails (disk full, permission, etc.), the staged file is left intact for operator inspection; the attachment is logged + skipped.

This scheme is the recommended default for plugins that already perform their own upstream fetch (auth headers, retries, format validation).

#### `https://<url>` (or `http://<url>`)

The plugin emits an upstream URL; the daemon fetches it and stores the result. Useful when:

- The plugin has no local filesystem (rare for subprocess plugins, more relevant for future sandboxed contexts).
- The upstream URL is canonical and the daemon's standard HTTP behavior (redirects, default User-Agent, no auth) suffices.

Constraints:

- Daemon fetches with a default 60-second timeout, follows redirects (max 5), default User-Agent (`yaad-index/<version>`).
- 4xx / 5xx response: attachment is logged + skipped; entity proceeds.
- The daemon does NOT cache the URL beyond what's already in `<vault>/<kind>/<id>.<role>.<ext>`. Re-fetch happens only when the plugin re-emits a different URI (or operator forces).
- HTTPS is preferred; plain `http://` is allowed but logged as a warning per attachment.

This scheme is appropriate when the URL is stable and authoritative. Plugins authenticating against private endpoints should NOT use this scheme: the daemon doesn't carry plugin credentials.

#### `base64://<padding-stripped-base64>`

The plugin includes the bytes inline in the URI. Useful when:

- The payload is tiny (recommended < 100 KB raw; not enforced).
- The plugin runs in a context with no filesystem (sandbox, future WASM runtime, etc.).
- Avoiding a tmp-file roundtrip is meaningful (rare).

Constraints:

- Base64 alphabet is RFC 4648 standard (no URL-safe variant; padding optional, daemon accepts both).
- Daemon decodes on receipt; bad base64 is logged + skipped.
- The encoded payload bloats the JSON ~33%; a 100 KB binary becomes a 133 KB JSON field. Plugins SHOULD prefer `file://` or `https://` for anything larger.

### 3. Vault placement

Resolved binary lands at:

```
<vault-root>/<kind>/<local-id>.<role>.<extension>
```

`<local-id>` is the local part of the entity ID — everything AFTER the `<kind>:` namespace prefix. Local IDs MUST be alphanumeric + dash + underscore only (`^[a-z0-9_-]+$`); kinds whose canonical IDs contain other characters (dots, slashes, colons) are out of scope for the attachment contract until they normalize their local-ID shape.

For entity ID `boardgame:130680` (kind `boardgame`, local-id `130680`) with role `thumb`, extension `jpg`, vault root `/path/to/vault`:

```
/path/to/vault/boardgame/130680.thumb.jpg
```

Sibling to the entity's `<vault>/<kind>/<local-id>.md`. The frontmatter of the .md file MAY reference the attachment (e.g., `image: 130680.thumb.jpg`) but the placement is canonical regardless of frontmatter; the daemon walks `<vault>/<kind>/<local-id>.*` to find all attachments, EXCLUDING `.md` (the entity file itself) and any future vault-native extensions reserved for the daemon's own use.

### 4. Re-fetch and preservation semantics

On re-ingest:

- If the plugin emits an attachment with a `(role, uri)` that matches an existing on-disk file (same role, same uri stamped in the entity provenance), the daemon SKIPS the fetch and leaves the existing file in place.
- If the plugin emits a different `uri` for the same `role`, the daemon performs the fetch and OVERWRITES the existing file.
- If the plugin emits no attachments on a re-ingest (or the array is missing entirely), existing on-disk attachments are PRESERVED. Same shape as UGC preservation per ADR-0012: a plugin's silence does not delete operator-visible artifacts.
- Operator-driven force-refetch (Per the prior design,'s `cache refetch`) refetches all attachments unconditionally.

The provenance entry on the entity records the URI(s) used at last fetch under a `fetch_attachments` field on the provenance row. Wire shape: an array of `{role, uri}` objects (mirrors the FetchResult shape minus `extension`, which is preserved on disk via the filename). Example:

```yaml
provenance:
 - source: bgg
 fetched_at: 2026-05-06T12:00:00+02:00
 ok: true
 fetch_attachments:
 - role: thumb
 uri: "https://cf.geekdo-images.com/.../thumb.jpg"
 - role: cover
 uri: "file:///tmp/yaad-index-staging/boardgame-130680-cover.jpg"
```

This shape lives ON the existing provenance row as a new optional field, NOT as a separate record. Provenance entries without `fetch_attachments` (legacy entities, or fetches that produced no attachments) are unchanged. The next ingest's re-fetch comparison is purely a string compare on `(role, uri)` against the freshest provenance entry's `fetch_attachments`.

### 5. Filename validation + path-traversal guard

The daemon performs three load-bearing checks BEFORE any filesystem access. All apply to every attachment regardless of URI scheme; failures log at WARN and the attachment is skipped (the rest of the entity proceeds).

#### 5a. Role allowlist

`role` MUST match `^[a-z0-9][a-z0-9-]{0,31}$`. This blocks path-traversal via the role field (e.g. a malicious plugin emitting `role: '../../etc/cron.d/evil'` is rejected before the filename is built). The regex also rules out shell metacharacters, whitespace, and Unicode shenanigans.

#### 5b. Extension allowlist

`extension` MUST match `^[a-z0-9]{1,10}$`. Blocks path-traversal via the extension field (e.g. `extension: 'jpg/../../etc/evil'` is rejected) and constrains to plausible file extensions. The 10-char cap covers all real-world extensions; longer is suspicious.

#### 5c. Path-traversal guard for `file://` URIs

For `file://` URIs, after the role + extension checks pass:

1. Resolve the URI to an absolute filesystem path.
2. Check the path is a strict descendant of the configured `plugin_staging_dir` (after symlink resolution).
3. Reject paths that escape the staging dir, contain `..` components after resolution, or point at non-regular files.

The destination path `<vault>/<kind>/<local-id>.<role>.<extension>` is also re-validated post-construction: it MUST be a strict descendant of `<vault>/<kind>` (defense-in-depth against a `local-id` somehow containing path separators despite the §3 constraint).

These three checks together close the path-traversal surface. A code-review checklist for any future ADR or daemon change touching attachment paths SHOULD verify all three are exercised.

### 6. Operator config

A new top-level config key:

```yaml
plugin_staging_dir: /tmp/yaad-index-staging # default: /tmp
```

Plugins receive this path via the `YAAD_PLUGIN_STAGING_DIR` environment variable (per the same plumbing as `YAAD_TIMEZONE` from ADR-0014's predecessor, yaad-index PR-D). Plugins SHOULD stage their tmp files under this dir.

The default `/tmp` is fine for single-operator deployments; operators wanting isolation (per-tenant staging, restricted permissions) override.

## Alternatives considered

### Alternative A: canonical single method (only `file://`)

**Rejected.** Removes flexibility for plugins without FS access (sandboxed contexts, future WASM runtimes). Forces a tmp-file roundtrip for tiny payloads where `base64://` would be cleaner. The simplification gain (one dispatch path) is real but small relative to the lost flexibility.

### Alternative B: stdin streaming for binary content

**Rejected.** Mixing binary bytes with the JSON response on stdout requires multipart-like delimiters or a separate stdin/stdout split. Both add parsing complexity and break the simple "JSON in, JSON out" contract that makes plugins easy to write and test. Tmp files achieve the same separation with less protocol surface.

### Alternative C: daemon-only fetch (force `https://` as the only scheme)

**Rejected.** Pushes the entire fetch responsibility to the daemon, which then duplicates HTTP logic each plugin already has (auth, retries, format negotiation, rate limits). Plugins authenticating against private endpoints (BGG_API_KEY, GitHub PAT, etc.) would have to expose those credentials to the daemon, breaking the credential-isolation property of subprocess plugins.

### Alternative D: plugin writes directly to vault

**Rejected.** Violates ADR-0008's vault-as-source-of-truth-with-daemon-as-sole-writer property. Concurrent writes from plugin and daemon's auto-commit pipeline race; the audit-commit log loses coherence. Solving these is more work than supporting three URI schemes.

## Consequences

### Daemon work

- Implement attachment dispatch: a single function that takes `(uri, dest)` and writes the resolved bytes to `dest`, dispatching on scheme.
- Implement re-fetch comparison: track the last-emitted URI per `(entity_id, role)` in provenance.
- Implement path-traversal guard for `file://`.
- Implement tmp cleanup for `file://`.
- Implement URL fetch with timeout for `https://`.
- Test matrix: 3 schemes × {happy path, missing source, malformed URI, traversal attempt, oversized payload}.

### Plugin SDK work

- Add a Go helper `attach.File(role, path, ext)`, `attach.URL(role, url, ext)`, `attach.Bytes(role, data, ext)` that produce the right wire shape. Plugins import this from the SDK rather than hand-rolling.
- Document the staging-dir env-var convention (`YAAD_PLUGIN_STAGING_DIR`).

### Security

- Path-traversal guard is the load-bearing protection; if it has a bug, a malicious plugin could write anywhere on disk. Test it directly with adversarial inputs.
- `https://` fetch SHOULD respect operator-level URL allowlists if the daemon already has them (open question; see "Open questions" below). For v1, no allowlist; trust the plugin's emission.
- `base64://` payloads are bounded by JSON size limits the daemon already enforces on FetchResult; no separate cap.

### Performance

- `file://` is hardlink-when-same-fs, copy-when-different. Cheap.
- `https://` is a network roundtrip per attachment, per re-fetch. Mitigated by the re-fetch-only-if-uri-changed rule.
- `base64://` is in-memory decode; bounded by FetchResult size limits. Cheap unless plugins abuse it.

## Open questions

These are punted to follow-up issues, not blockers for the v1 contract:

1. **URL allowlist.** Should `https://` URIs pass through an operator-configured allowlist (e.g., only `*.geekdo-images.com` for the BGG plugin)? Pro: defense-in-depth against plugin compromise. Con: every plugin needs a configured allowlist, friction. v1: no allowlist.
2. **Content-type validation.** Should the daemon validate the fetched bytes match the declared `extension` (e.g., reject if it asked for `jpg` and got HTML)? v1: no validation; plugin is responsible.
3. **Streaming large files.** Files > 100 MB are practical for some plugins (audio archives, video). Tmp+URI handles this in principle but doesn't optimize. Future ADR if needed.
4. **`s3://` and other schemes.** The contract is extensible; new schemes can be added without breaking existing plugins. Add when a real plugin needs it.

## Migration

This is an additive change to the FetchResult schema. Plugins that don't emit attachments are unaffected. yaad-wikipedia (no attachments today) requires no change. yaad-bgg PR-B (the trigger for this ADR) is the first consumer and will land after this ADR.

No frontmatter migration required (attachments live alongside `.md`, not inside it).
