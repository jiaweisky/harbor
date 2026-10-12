# Harbor storage extension (v1)

URI: `https://github.com/jiaweisky/harbor/extensions/storage/v1`

When Harbor is configured with a storage backend, its AgentCard advertises this
extension. Its `params.ingest` object is the contract a sender uses to hand
Harbor a file out of band: the HTTP response carries only a reference (a URI),
while the bytes travel over the storage backend. See STORAGE-DESIGN.md for the
design.

## Ingest contract (server → client)

```json
{
  "ingest": {
    "schemes": ["https", "s3"],
    "mediaTypes": ["text/*", "application/json", "application/xml", "image/*"],
    "maxInlineBytes": 1048576,
    "maxBytes": 268435456,
    "modes": [
      { "mode": "reference",
        "upload": { "base": "https://store.example.com/in/harbor/", "method": "PUT",
                    "auth": "none", "key": "{sha256}",
                    "reference": "https://store.example.com/pub/harbor/" } },
      { "mode": "broker" }
    ]
  }
}
```

| field | type | meaning |
|---|---|---|
| `ingest.schemes` | string[] | Reference URI schemes Harbor accepts. |
| `ingest.mediaTypes` | string[] | Allowed ingest media types (patterns; `text/*`). |
| `ingest.maxInlineBytes` | int | Largest inline `FilePart`; above it, use a reference. |
| `ingest.maxBytes` | int | Largest accepted reference. |
| `ingest.modes` | object[] | Supported ingest capabilities; a client picks one. |

Each mode is one of:

- `reference`: `upload` gives a static target. The sender forms `base + key`
  (where `key` is rendered from the `key` rule, e.g. `{sha256}`), PUTs the bytes
  there, and sends `FilePart(url = reference + key)`. `reference` defaults to
  `base`; `auth` is `none` for an advertised target.
- `broker`: the sender reserves a per-file target (below), because the store has
  no open-write prefix.

## Broker exchange (client ↔ server)

A `message/send` whose content is a `storage/upload-slot-request` DataPart is
answered with a completed task carrying a `storage/upload-slot` DataPart.

Request:

```json
{ "kind": "storage/upload-slot-request",
  "name": "report.pdf", "size": 1048576,
  "mediaType": "application/pdf", "sha256": "..." }
```

Response:

```json
{ "kind": "storage/upload-slot",
  "uploadUrl": "https://store.example.com/...presigned-put...",
  "method": "PUT",
  "headers": { "Content-Type": "application/pdf" },
  "reference": "s3://bucket/harbor/ab/abcdef.../report.pdf",
  "expiresAt": "2026-10-11T10:00:00Z" }
```

The sender PUTs the bytes to `uploadUrl` with `headers`, then sends
`FilePart(url = reference)`. A request whose size or media type is out of policy
is rejected with `harbor.errorCode = "unsupported_content"`.

## Caller-provided ingest (client → server)

Because A2A does not expose the caller's card, a caller that wants response files
larger than the inline threshold includes its own ingest target in the request
metadata as `harbor.ingest`, shaped like a `reference` `upload` object:

```json
{ "harbor.ingest": {
  "base": "https://caller.example/in/",
  "method": "PUT",
  "key": "{sha256}",
  "reference": "https://caller.example/pub/",
  "maxInlineBytes": 1048576
} }
```

Harbor then uploads oversized artifacts to `base + key` and emits
`FilePart(url = reference + key)`; without `harbor.ingest` it inlines instead.
