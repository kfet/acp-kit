# Out-of-band file transfers for relays

`mcphost` is reached over stdio through a local unix socket, so it has no
https endpoint of its own. The MCP files extension
(`io.modelcontextprotocol/files`) only lets a stdio server hand out absolute
`https` targets with `credentials: "omit"`. Chat platforms already host files
at such URLs, so the relay supplies them through two optional `command`
capabilities. A relay that implements neither behaves exactly as before
(inline `send_file` only).

## Protocol shape (what the agent's client sees)

Matches fir ≥ v1.52.0 (`pkg/mcp/files*.go`).

- `server/discover` returns `supportedVersions: ["2026-07-28"]`. Clients
  enable the extension only on that revision. The response also carries
  `capabilities.extensions["io.modelcontextprotocol/files"] =
  {maxInlineBytes, upload: {}}`. `upload` is present only when an
  `UploadTargeter` is registered.
- The client sends a large file's `FileInput` with no `data`. The Host answers
  `{"resultType": "upload_required", "uploads": [{fileId, url, method,
  headers?, form?: {fields: [{name, value}], fileField}, credentials: "omit",
  expiresAt?}], "requestState": "<signed>"}` and does **not** run the tool.
- The client uploads, then retries the same call with `requestState`. The Host
  checks the state, asks the relay to finish each upload, and runs the tool
  with `Call.Files[param] = UploadedFile{Ref, Name, MimeType, Size}`.
- A tool result can carry `_meta["io.modelcontextprotocol/files"].downloads =
  [{url, name, mimeType, size, digest?, headers?, credentials: "omit",
  expiresAt?}]`. Downloads go only to clients that declare `download` in the
  request's `_meta` clientCapabilities. Other clients get a text note instead.

### requestState

- **Format:** `base64url(JSON payload) "." base64url(HMAC-SHA256)`. The
  payload binds the tool name, the SHA-256 of the session key, the SHA-256 of
  the canonical arguments (key order does not matter), each file's
  `(param, id, ref)`, an expiry (`mcphost.UploadStateTTL`, 30 min) and a
  random nonce.
- **Single use:** a used nonce is refused. A state from another session, for
  other arguments or for another tool is also refused.
- **Restarts:** the HMAC key and the used nonces travel in
  `ExportTokens`/`SeedTokens`, so a state issued before a re-exec still works
  after it, once.
- **Readable by the client:** the payload is signed, not encrypted. Never put
  a secret in `UploadTarget.Ref`; it is capped at 1 KiB.

### Target rules enforced by mcphost

- The URL must be absolute `https` with no userinfo.
- The method must be PUT or POST. A form upload needs POST and a `fileField`.
- These headers are refused: `Authorization`, `Proxy-Authorization`,
  `Cookie`, `Host`, `Content-Length`, the method overrides, `Sec-*` and
  `Proxy-*`. With a form, `Content-Type` is refused too.
- A claimed `digest` is passed to the provider in `UploadSpec.Digest`.
  mcphost never sees the bytes, so checking it is the provider's job.
- Size is checked after the upload: `FinishUpload`'s size must equal the size
  the client claimed. `accept` is checked again against the stored type.

**The relay's own API credentials must never appear in a target.** The URL
itself is the bearer capability. Make it single-use, scoped to one file and
short-lived.

## Interfaces

```go
// command: upload big files straight to the platform (send_file)
type UploadTargeter interface {
    UploadTargetFor(convID string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error)
    FinishUploadFor(convID string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error)
    PostUploadedTo(convID string, f mcphost.UploadedFile, caption string) ([]string, error)
}

// command: expose inbound attachments (list_attachments / fetch_attachment)
type DownloadLinker interface {
    AttachmentsIn(convID string) ([]command.InboundAttachment, error)
    DownloadLinkFor(convID, attachmentID string) (mcphost.DownloadTarget, error)
}
```

**Wiring:** implement either interface on the Controller or the Poster, or
pass it to `Broker.SetUploadTargeter` / `Broker.SetDownloadLinker`. Then call
`relaytool.Tools.Register(host)`. It installs the upload provider
(`Host.SetUploads`) and registers the tools with `Host.ToolCall`.

**Inline files still need `FilePoster`:** keep `FilePoster` for small files,
which still arrive inline (up to `maxInlineBytes`). `send_file` is listed when
either capability is present. Without `FilePoster`, inline sends fail.

**Custom tools:** register with `Host.ToolCall` and return
`mcphost.Result{Downloads: ...}` to hand back download targets.

## Per-platform recipes

### Slack: native fit

- **`UploadTargetFor`:** call `files.getUploadURLExternal(filename, length)`
  with the bot token, **server-side**. Return `UploadTarget{URL: upload_url,
  Method: "POST", Ref: file_id}`. The returned `upload_url` is pre-signed and
  needs no auth header, so no credential reaches the client. Use a plain body
  POST, or `Form{FileField: "file"}`.
- **`FinishUploadFor`:** check the file with `files.info(file_id)` if needed,
  then return `UploadedFile{Ref: file_id, Size: spec.Size}`. Slack does not
  report a size before completion, so trust the size the client claimed.
  `completeUploadExternal` fails if the bytes never arrived.
- **`PostUploadedTo`:** call `files.completeUploadExternal(files=[{id, title}],
  channel_id, thread_ts, initial_comment=caption)`.
- **Downloads:** `url_private` needs the bot token, so do **not** hand it
  out. Two options:
  - Make the file public with `files.sharedPublicURL` and give out its
    `permalink_public`. This changes the file's visibility.
  - Better: serve a relay-hosted signed proxy URL, `https://relay/dl/<hmac
    token>`. The relay fetches `url_private` with the bot token and streams it
    to the client.

### Zulip

- **Uploads:** `POST /api/v1/user_uploads` needs the bot's basic-auth
  credentials, so it **cannot** be handed out. Mint a one-shot relay endpoint
  instead:
  - `UploadTargetFor` returns
    `UploadTarget{URL: "https://relay.example/up/<random>", Method: "PUT",
    ExpiresAt: now+10m, Ref: "<random>"}`.
  - The relay's https listener (behind its reverse proxy or a share) accepts
    exactly one PUT of at most `spec.Size` bytes for that token. It then
    forwards the bytes to `/user_uploads` with the bot credentials, and keeps
    the returned `/user_uploads/...` path under the token.
- **`FinishUploadFor`:** look up the token. Fail if nothing arrived. Return
  `UploadedFile{Ref: path, Size: n}`.
- **`PostUploadedTo`:** send a message `[name](/user_uploads/...)` plus the
  caption to the topic.
- **Downloads:** call `GET /api/v1/user_uploads/{realm_id}/{filename}` with
  the bot credentials. It returns a temporary signed `url` that works for
  about 60 s without auth. Return that as `DownloadTarget.URL` with the exact
  size, which you get from a HEAD on the signed URL. Because it expires
  quickly, mint it in `DownloadLinkFor`, at fetch time.
- **Attachments:** `AttachmentsIn` parses the `/user_uploads/...` links in the
  conversation's recent messages.

### Poe

- **Uploads:** Poe bots receive attachments but there is no bot-side upload
  URL that a third party can PUT to. Options:
  - Keep `send_file` inline-only (`FilePoster` via the
    `file_event`/attachment API).
  - Use the same one-shot relay-hosted PUT endpoint as Zulip, and have
    `PostUploadedTo` post the stored bytes as an attachment.
- **Downloads:** inbound `attachments[].url` in a Poe query is already a
  public https URL, with no auth and an expiry set by Poe. `AttachmentsIn`
  lists the conversation's recent attachments (`name`, `content_type`).
  `DownloadLinkFor` returns `{URL: url, Name, MimeType, Size}`. Poe gives no
  size, so do a HEAD request for `Content-Length` first.

### Relay-hosted endpoint checklist (Zulip, Poe, Slack downloads)

- Use a random 128-bit token per file and an expiry of 15 minutes or less.
- Accept a single use. Cap the body at the claimed size and check the length
  is exact.
- Serve over https only (agentbox `Share`, or a reverse proxy).
- Never forward the client's headers to the platform. Add the bot
  credentials only on the relay → platform hop.
