# Harbor File Exchange and Storage Data Plane

Status: design proposal for review. Not yet implemented.

This document specifies how Harbor moves large files between agents. It separates
the A2A control plane (which prefers references and carries only small inline
files) from a configurable storage data plane (which carries the bytes). The
design is deliberately small: one configured storage backend per Harbor process,
no cross-backend routing. The storage contract is advertised on the AgentCard
(section 7).

## 1. Problem

Harbor exposes coding-agent CLIs as A2A agents. Agents frequently need to
exchange large files (text, images, video, audio). Today every file travels
inside the A2A message or artifact, which causes two independent problems.

### 1.1 P1: transport efficiency

- A2A `Part` raw content is base64-encoded inside JSON-RPC, inflating payloads
  by roughly 33%, and each message or SSE event is buffered whole.
- The outbound paths hard-cap file artifacts: 8 MiB per image artifact and 1 MiB
  per per-file artifact, and files above the cap are silently dropped
  (`internal/bridge/executor.go` `readChangedFile`). Video and audio cannot be
  returned at all. None of these limits are advertised on the AgentCard, which
  lists only MIME types, so a client cannot learn them and instead discovers a
  limit by finding a file missing (see section 7.2).
- Large inline artifacts are also persisted with the task and replayed when a
  duplicate `messageId` arrives (`replayDuplicate`), bloating the store.

### 1.2 P2: context pollution

File content can still land in a model's context instead of staying a handle:

- `internal/bridge/prompt.go` `appendDataBlock` expands a `DataPart` into a
  prompt text block regardless of size.
- `internal/bridge/agent/image.go` sends accepted images as native base64 image
  content blocks: intended for vision, but they cost tokens rather than staying a
  handle.
- On the receiving side, Harbor's outbound artifacts are inline bytes, so a peer
  that feeds message content to its model ingests whole files.

### 1.3 Root cause

A2A `FilePart` already has two forms: inline bytes (`a2a.NewRawPart`) and a URI
reference (`a2a.NewFileURLPart`). The part itself always travels over A2A; only
the bytes take a path, either inline inside the message or out of band via the
URI. A2A standardizes the shapes but not the policy: it does not say when to
inline versus reference, where a reference points, how to make it reachable and
authorized, or how the two sides agree on a channel. Harbor's implementation
uses only the inline form, so its outbound always pushes bytes into the message
and a client has no reference option to choose. On the way in it has the mirror
gap: with no attach mechanism it writes a host-local path (`materializeFile`)
into the prompt, which is a reference meaningful only on Harbor's own host and so
neither delivers the file nor is portable.

## 2. Goals and non-goals

Goals:

1. Large files travel as URI references over a shared, configurable storage
   backend; only small files may remain inline, under the sender's own local
   default (not an advertised contract).
2. Each Harbor process configures exactly one storage backend (its own ingest
   store). Peers need not match it: a sender uploads to the target the receiver
   advertises in its AgentCard, and the receiver reads its own store locally, so
   no cross-backend routing is needed.
3. The consumer materializes a reference into a local file and hands the CLI a
   path via the existing `file_args`/`image_args`, so file bytes are not dumped
   into the prompt text (only under `inbound_media: inline` is media projected
   natively); the model gets a handle.
4. The storage contract is discoverable from the AgentCard, so a peer does not
   need out-of-band knowledge to exchange files.
5. With no storage configured, behavior is identical to today.

Non-goals:

Out of scope (not part of this layer):

- Native SaaS document integration (Google Docs, Tencent Docs, Weiyun):
  creating, exporting in native formats, and uploading inside those services
  need their OAuth/App APIs and are separate connectors. A plain public HTTPS
  link they expose is still fetchable by the `http` backend; only the native
  integration is out of scope.
- Spooling large `DataPart`s: structured data is separate, so a large `DataPart`
  can still be inlined into the prompt (`appendDataBlock`); this design does not
  close that vector.
- End-to-end encryption, content scanning, and any peer authorization beyond the
  store's own: the store authenticates access, and Harbor adds no layer above it.

Deferred until a trigger (see section 18): Range and media streaming, and
multipart upload.

## 3. Architecture

```
control plane (A2A)                    data plane (Storage)
-------------------                    --------------------
Message / Artifact   -- reference -->  sender uploads to the
  FilePart(url=...)    (large files)     receiver's target;
  FilePart(raw)        (small files,     receiver Store.Get reads
                        inline)          it back (http/s3/cos/local)
```

- The storage service is the hub in the network topology. If A, B, and C can all
  reach the store, they can exchange files even when they cannot reach each
  other (partial mesh).
- The receiver's contract is mandatory: its `ingest` limits (`maxInlineBytes`,
  `maxBytes`, `mediaTypes`, `modes`) tell a sender how it may deliver. The
  sender's own inline threshold is optional and local — a sending agent decides
  per case whether to inline or reference, within the receiver's limits. The
  `FilePart` travels over A2A either way, so a reference must be self-describing:
  its scheme is enough for the receiver to locate the backend.
- Receiver-driven upload (the only supported model): a sender learns how to
  upload from the receiver's `ingest` contract — a static target or a brokered
  per-file target, with the accepted schemes, sizes, and media types. The sender
  uploads there and sends the reference; the receiver reads its own store
  locally. A process configures exactly one backend, and peers need not match it.
- If a receiver's card describes no file transfer, the sender assumes nothing
  about how the receiver can receive files and uses only A2A's baseline (inline
  bytes). The receiver may support another means that is simply not advertised;
  the sender does not assume it cannot. Because A2A does not expose the caller's
  card, a caller that wants response files advertises its own `ingest` contract
  (or card URL) in the request.

## 4. Core abstraction

New package `internal/blob`. The abstraction is the receiver's store: it reads
back ingested files and issues the upload targets its AgentCard advertises.

```go
package blob

// ErrUnsupportedURI reports a URI a store cannot handle.
var ErrUnsupportedURI = errors.New("unsupported uri")

// UploadTarget is offered to a sender: the URL, method, and headers to upload
// with, and the reference to put in the A2A FilePart afterwards.
type UploadTarget struct {
    URL       string
    Method    string
    Headers   map[string]string
    Reference string
    Expires   time.Time
}

// Store is this process's own file backend: it serves the process's ingest.
type Store interface {
    // Get reads uri back into bytes. It returns ErrUnsupportedURI when the URI's
    // scheme or host is not this store's.
    Get(ctx context.Context, uri string) (io.ReadCloser, error)
    // Target returns the static upload target advertised on the card (reference
    // mode); the zero value when the store has none.
    Target() UploadTarget
    // Reserve issues a per-file upload target (broker mode) for the named file.
    Reserve(ctx context.Context, name, mediaType string, size int64) (UploadTarget, error)
    // Put writes a file into the store itself. It backs the process's ingest
    // endpoint when a sender PUTs to it; under D2/D3 the bytes reach the store
    // directly and only Get is used.
    Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (uri string, err error)
}
```

Properties:

- The receiver never dispatches: it reads back its own store. `Target`/`Reserve`
  are how it advertises and how a broker issues a per-file slot.
- The sender side is separate: uploading to an advertised `UploadTarget` is a
  small scheme-keyed uploader (https PUT today), not part of `Store`.
- No registry, no dispatcher, no writer pool. `CLIExecutor` holds one
  `blob.Store`, which may be nil (storage disabled).

## 5. Configuration

A top-level block, shared by all agents (peer of `server` and `sandbox`).
`config.Config` gains a `Storage StorageConfig` field.

```yaml
storage:
  backend: http            # http | s3 | cos | local (exactly one)
  prefix: harbor/          # key prefix within the store
  inline_max_bytes: 262144 # sender's local default: <= this inline, > this reference; not advertised
  media_types:             # allowed ingest media types (example); unset defaults to text/*, application/json, application/xml, image/*
    - text/*
    - application/json
    - application/xml
    - image/*
    - video/*
    - audio/*
  inbound_media: spool     # how an inbound attachment reaches the model: spool | inline
  download:                # reading ingested objects back from the store
    max_bytes: 268435456   # per-object limit
    timeout: 60s
    credentials:                   # credentials for a private HTTPS store origin
      - host: store.internal
        auth: bearer               # bearer | basic
        token: ${HARBOR_STORE_TOKEN}
  http:
    put_url: https://store.example.com/x/     # advertised upload base (reference mode)
    get_base: https://store.example.com/pub/  # read base for the store's own objects
    auth: none                                # none | basic | bearer; Harbor's own store credential (never advertised)
    username: ...
    password: ...
    token: ...
    tls:                                      # client side: how Harbor reaches the store over HTTPS
      ca_file: ""                             # trust a self-signed/private-CA store cert; empty = system trust store
      cert_file: ""                           # client certificate, only when the store requires mTLS
      key_file: ""                            # client private key
      insecure_skip_verify: false             # test only; never in production
    presign_url: ""                           # broker: signing service for a self-authenticating target
    presign_secret: ""                        # or sign locally (for example nginx secure_link)
    presign_ttl: 15m
  s3:
    endpoint: https://cos.ap-guangzhou.myqcloud.com
    region: ap-guangzhou
    bucket: my-bucket
    access_key: ${AWS_ACCESS_KEY_ID}
    secret_key: ${AWS_SECRET_ACCESS_KEY}
    session_token: ${AWS_SESSION_TOKEN}       # optional, temporary credentials
    path_style: false
    presign_ttl: 15m
  cos:
    region: ap-guangzhou
    bucket: examplebucket-1250000000
    secret_id: ${COS_SECRET_ID}
    secret_key: ${COS_SECRET_KEY}
    session_token: ${COS_SESSION_TOKEN}       # optional, temporary credentials
    endpoint: ""                              # default <bucket>.cos.<region>.myqcloud.com
    presign_ttl: 15m
  local:
    root: /shared/harbor                      # shared directory (same host or NFS)
    serve_base_url: ""                        # optional; read root over HTTP without the mount
```

The advertised upload target is derived from the backend: `http.put_url`,
`local.root` (with `serve_base_url`), or `s3://bucket/<prefix>` /
`cos://bucket/<region>/<prefix>` for the object stores. It is what a peer sees in the card, so it carries no secret: reference
mode advertises `auth: none`, and an upload that needs authentication uses broker
mode, whose presigned target is self-authenticating. A native scheme in
`ingest.schemes` (`s3://`, `cos://`) means a sender must hold matching
credentials, i.e. a shared trust domain (D2); across domains, advertise `https`
targets (public or presigned) instead. The single top-level `prefix` applies to
every backend.

Transport security and authentication:

- `storage.http.tls` is the **client side** — how Harbor reaches the store.
  `ca_file` trusts a self-signed or private-CA store certificate; it is empty for
  a public (authoritative) CA, where the system trust store already holds the
  root. `cert_file`/`key_file` are Harbor's **client certificate**, needed only
  when the store requires mutual TLS; `insecure_skip_verify` is for tests only.
- `server.tls` is the **server side** — when Harbor itself serves HTTPS (its A2A
  endpoint, or its own ingest endpoint at `<base_url>/agents/<id>/storage/in/`):

  ```yaml
  server:
    tls:
      cert_file: /etc/harbor/server.crt   # Harbor's server certificate
      key_file:  /etc/harbor/server.key
      ca_file:   ""                        # verify a peer's client certificate
      client_auth: false                   # require a client certificate (mTLS)
  ```
- `http.auth` is Harbor's own credential for reaching the store (`none`, `basic`,
  or `bearer`; default `none`), with `username`/`password` for basic and `token`
  for bearer. It is never advertised: the card's `ingest.upload.auth` is always
  `none`, and an authenticated upload uses broker mode (a self-authenticating
  presigned target).

Environment variables: the config supports `${NAME}` and `${NAME:-default}`
interpolation from the process environment, applied to the raw file before YAML
decoding (`config.Load`). Store credentials and signing secrets (for example
`${COS_SECRET_KEY}`, `${HARBOR_STORE_TOKEN}`) are supplied this way instead of
being committed to the file; a literal `${` is written `$${`. This is separate
from `agents[].env`, which sets the environment of the spawned CLI rather than
substituting into the config.

Validation (`config.validate`):

- `backend` must be one of `http`, `s3`, `cos`, `local`; then its required fields
  are checked.
- `inline_max_bytes > 0` and `download.max_bytes > 0`.
- `inbound_media` must be one of `spool`, `inline` (default `spool`).
- `media_types` is an allowlist of media-type patterns (`type/subtype`, with
  `*` subtype, case-insensitive). Unset defaults to `text/*`,
  `application/json`, `application/xml`, and `image/*`; `*/*` allows every type.
  An ingest whose effective media type is not in the list is rejected with
  `unsupported_content`.
- A missing `storage` block is valid: `Store == nil`, and the degraded path is
  used.

Auth material (keys, tokens, signatures) is per-backend configuration and
deployment input, not a design dimension.

## 6. Storage backends

A storage backend is one configured `blob.Store` implementation: `http`, `s3`,
`cos`, or `local`. A Harbor process configures exactly one, so no routing is
required.

The backend is the receiver's own store; how it authenticates is a deployment
input and does not change the delivery patterns (section 7.1). The four backends
differ only by configuration.

| storage backend | URI shape | Auth combinations covered | Notes |
|---|---|---|---|
| `http` | `get_base + key` (public) or a signed URL (presigned) | upload public or signed; read public or signed | Most portable; Put via HTTP PUT or a signing service, Get is a plain HTTPS GET |
| `s3` | `s3://bucket/prefix/key` or a presigned HTTPS URL | upload signed; read signed or presigned | S3-compatible (S3, OSS, MinIO); COS also works here via SigV4 |
| `cos` | `cos://bucket/region/key` (native) or a presigned HTTPS URL | upload signed; read signed or presigned | Tencent COS native REST API (`q-sign-algorithm=sha1`), no S3 SDK |
| `local` | `file:///shared/harbor/key` | neither side signed | Shared directory; optional `serve_base_url`; same host or NFS only |

- A public URL and a presigned URL are both, on the consumer side, a plain
  HTTPS GET. Only a bare native reference (`s3://...`) needs matching
  credentials on the consumer.
- An unhandled URI returns `blob.ErrUnsupportedURI`, mapped to the A2A
  `unsupported_content` error code.

Each backend has a subsection below; `local` (section 6.4) is the degenerate
shared-filesystem case and the zero-dependency dev/test backend.

### 6.1 Tencent COS (`cos`)

`cos` drives Tencent COS over its **native REST API** rather than the
S3-compatible one, so it needs no S3/SigV4 implementation. It signs with the COS
SHA1 scheme (`q-sign-algorithm=sha1`), verified against the official signature
reference and the `cos-go-sdk-v5` `auth.go`:

- `sign_key = HMAC-SHA1(secret_key, key_time)` (hex)
- `http_string = method + "\n" + uri_pathname + "\n" + http_params + "\n" + http_headers + "\n"`
- `string_to_sign = "sha1\n" + key_time + "\n" + SHA1(http_string) + "\n"` (hex)
- `signature = HMAC-SHA1(sign_key, string_to_sign)` (hex)

`Put` sends `PUT /<key>` with an `Authorization: q-sign-algorithm=sha1&q-ak=...&
q-sign-time=...&q-key-time=...&q-header-list=...&q-url-param-list=...&q-signature=...`
header (optionally with `Content-Type` / `Content-MD5`, which then join the
signed headers). `Get` sends `GET /<key>` with the same header. Presigning
appends the `q-sign-*` components as **URL query parameters** (`q-sign-algorithm`,
`q-ak`, `q-sign-time`, `q-key-time`, `q-header-list`, `q-url-param-list`,
`q-signature`), URL-encoded with `;` as `%3B`; this is the presigned form, not
`?sign=...` (which is the POST Object form field).

Details to get right, per the reference and Go SDK:

- Only the signed-header whitelist participates (`host`, and on upload
  `content-type` / `content-md5`, plus `x-cos-*`); a minimal GET/PUT signs
  `host` alone.
- The canonical `uri_pathname` is the request path as the SDK uses it
  (`URL.Path`); entity values use COS's `safeURLEncode`, which also encodes
  `! ' ( ) *`.
- A COS presigned URL is a **simple upload only**: one object, at most 5 GiB, no
  multipart. A larger upload needs multipart and, in broker mode, a negotiated
  part flow.
- Temporary credentials are supported via `x-cos-security-token` (header, or
  query param when presigned); the reference recommends temporary credentials
  for presigning.

Config:

```yaml
  cos:
    region: ap-guangzhou
    bucket: examplebucket-1250000000
    secret_id: ${COS_SECRET_ID}
    secret_key: ${COS_SECRET_KEY}
    session_token: ${COS_SESSION_TOKEN}   # optional, temporary credentials
    endpoint: ""                          # default <bucket>.cos.<region>.myqcloud.com
    presign_ttl: 15m
```

### 6.2 AWS S3 and S3-compatible services (`s3`)

`s3` signs with AWS Signature Version 4 (SigV4), the standard for AWS S3 and the
S3-compatible interfaces of Tencent COS, Alibaba OSS, MinIO, and Ceph. It needs
no SDK; the four steps of the AWS reference are:

- Canonical request: `method\ncanonical_uri\ncanonical_query\ncanonical_headers\nsigned_headers\nhashed_payload`
- String to sign: `AWS4-HMAC-SHA256\n<amz-date>\n<date>/<region>/s3/aws4_request\nSHA256(canonical_request)`
- Signing key: `kDate=HMAC("AWS4"+secret,date)`, `kRegion=HMAC(kDate,region)`,
  `kService=HMAC(kRegion,"s3")`, `kSigning=HMAC(kService,"aws4_request")`
- Signature: `Hex(HMAC(kSigning, string_to_sign))`, added as
  `Authorization: AWS4-HMAC-SHA256 Credential=<ak>/<scope>, SignedHeaders=<...>, Signature=<...>`

`Put` sends `PUT /<key>` with the Authorization header and `x-amz-content-sha256`
(the payload's SHA-256); `Get` sends `GET /<key>` with the empty-string hash
(`e3b0c442...b855`). Presigning appends `X-Amz-Algorithm`, `X-Amz-Credential`,
`X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders`, and `X-Amz-Signature` as
query parameters, with the literal `UNSIGNED-PAYLOAD` as the payload hash.

Details to get right:

- `UriEncode`: encode every byte except `A-Za-z0-9-_.~`; encode `/` everywhere
  **except in the object key path**; space as `%20`; uppercase hex. The canonical
  query string sorts by encoded key name.
- `CanonicalHeaders`: lowercase names, trimmed, sequential spaces collapsed to
  one, sorted; include `host`, `content-type` when sent, and every `x-amz-*`
  header.
- `x-amz-content-sha256` is required for S3 and is part of the signed headers.
- Virtual-hosted style (`https://<bucket>.<endpoint>/<key>`) is the default; set
  `path_style: true` for services that require path style (MinIO, some on-prem).
- A presigned URL is a simple upload, with a maximum expiry of 7 days. Temporary
  credentials add `X-Amz-Security-Token`.
- A header-signed request is rejected if the client clock is more than 15
  minutes from the service.

Config:

```yaml
  s3:
    endpoint: s3.amazonaws.com                # or cos.ap-guangzhou.myqcloud.com, minio host
    region: us-east-1
    bucket: my-bucket
    access_key: ${AWS_ACCESS_KEY_ID}
    secret_key: ${AWS_SECRET_ACCESS_KEY}
    session_token: ${AWS_SESSION_TOKEN}        # optional, temporary credentials
    path_style: false
    presign_ttl: 15m
```

### 6.3 Self-hosted HTTP store (nginx / OpenResty)

This is the `http` backend: a plain HTTP store you run yourself. nginx/OpenResty
cover the common cases with built-in modules or a little Lua.

- Get: serve a directory with `root`/`alias`. Access is public, or
  basic/bearer (`auth_basic`, or `auth_request` to a token check), or a
  time-limited signed link via `ngx_http_secure_link_module`:

  ```
  location /store/ {
      root /data;
      secure_link $arg_md5,$arg_expires;
      secure_link_md5 "$secure_link_expires$uri$remote_addr secret";
      if ($secure_link = "") { return 403; }
      if ($secure_link = "0") { return 410; }   # expired
  }
  ```

  `secure_link` signs with `base64url(MD5(expression))`, with the md5 in
  `$arg_md5` and the expiry (Epoch seconds) in `$arg_expires`; the
  `secure_link_secret` mode is the simpler `/prefix/hexmd5/link` form.
- Put: write with `ngx_http_dav_module`:

  ```
  location /in/ {
      alias /data/in/;
      dav_methods PUT;
      create_full_put_path on;                    # create intermediate dirs
      dav_access user:rw group:rw all:r;
      client_body_temp_path /data/client_temp;    # same filesystem as the store
      client_max_body_size 5g;
      limit_except PUT { deny all; }
  }
  ```

  OpenResty can replace this with a `content_by_lua_block` handler that checks a
  token and streams the body to disk (`lua-resty-upload` for multipart).
- Presign: `secure_link` for reads (Harbor computes the md5 from a shared
  secret, or calls `presign_url`); a Lua handler can mint HMAC-signed URLs when
  MD5 is too weak.
- TLS: present the store's server certificate in nginx; Harbor trusts a
  self-signed or private-CA certificate via `storage.http.tls.ca_file` (empty for
  a public CA) and presents its client certificate when nginx requires client
  authentication:

  ```
  server {
      listen 443 ssl;
      ssl_certificate     /etc/nginx/certs/server.crt;   # store server certificate
      ssl_certificate_key /etc/nginx/certs/server.key;
      ssl_verify_client   on;                            # mTLS (optional)
  }
  ```

Mapping to the `http` config:

- `get_base` = the read base (`/store/`).
- `put_url` = the advertised write base (`/in/`), with `auth: none`.
- Authed writes are not advertised; they use broker mode (`secure_link`-style
  presigned targets).
- `auth` = Harbor's own credential for reaching the store; `basic`/`bearer` set
  `username`/`password` or `token`.
- `tls` = the client side to nginx: `ca_file` trusts the store's server
  certificate, `cert_file`/`key_file` are Harbor's client certificate for mTLS.
- `presign_url` (optional) = a signing endpoint; for `secure_link` Harbor can
  sign locally from a shared secret.
- `presign_ttl` = the signed-link lifetime.

Caveats:

- `client_max_body_size` defaults to 1m; raise it or large uploads fail with
  `413`.
- Keep `client_body_temp_path` and the store on the same filesystem, so a `PUT`
  is a rename rather than a cross-filesystem copy.
- Both `ngx_http_dav_module` and `ngx_http_secure_link_module` require build
  flags (`--with-http_dav_module`, `--with-http_secure_link_module`).
- `secure_link` is MD5-based, not a general HMAC; use OpenResty Lua when a
  stronger signature is required.
- Constrain WebDAV `PUT` with auth and a fixed prefix; `create_full_put_path` can
  create directories, so an unauthenticated store root is an open write.

### 6.4 Local shared directory (`local`)

`local` is the no-server case: Harbor writes and reads files in a shared
directory and references them as `file:///...` URIs. It has nothing to configure
beyond `root` and an optional `serve_base_url`.

- Put writes `<root>/<key>`; Get reads it back; a produced reference is a
  `file://` absolute path.
- It is meaningful only when the peers share a filesystem: single-machine
  dev/demo/test, same-host agents, or a shared NFS/SMB mount. It is local to one
  host, not network-transparent, and not for untrusted peers.
- Relationship to section 6.3: a mounted directory served by nginx is the same
  objects over HTTP. Use `local` when a shared mount already exists and you want
  no server; use `http` (section 6.3) for cross-host access without a mount, or
  an object store (sections 6.1/6.2) for scale.
- `serve_base_url` is optional: when set, Harbor reads the directory over HTTP
  rather than the mount. Senders still need write access (the mount, or the
  `http` backend).
- Confinement: `Get` must stay within `root` — reject traversal and any absolute
  path outside it, and reject keys from another context or task. An inbound
  `file://` handled by any other backend returns `unsupported_content`
  (section 11).

Config:

```yaml
  local:
    root: /shared/harbor        # shared directory (same host or NFS)
    serve_base_url: ""          # optional; read root over HTTP without the mount
```

## 7. Storage discovery and delivery patterns

A peer must be able to learn how to hand Harbor an input file. Discovery is
advertised on the AgentCard: the storage extension declares Harbor's `ingest`
contract — a static target the sender uploads to directly (reference mode), or a
reserve operation the sender calls to obtain a per-file target (broker mode). A
sender reads the same contract for any Harbor agent it sends a file to.

### 7.1 Delivery patterns

Every transfer uses the receiver's ingest contract; only these patterns are
supported.

| Pattern | Mechanism | Discovery | Use |
|---|---|---|---|
| **D1 inline** | bytes inside the A2A message | none | small files; the baseline when the receiver advertises no `ingest` |
| **D2 shared store** | the sender writes to the shared store named by the receiver's `ingest.upload`, then sends the URI | receiver's `ingest.upload` | robust under partial mesh; both sides reach the store |
| **D3 brokered slot** | the sender asks the receiver for a write target; the receiver returns a presigned PUT into its store; the sender uploads, then references it | per-transfer handshake (section 7.5) | natural when the receiver is Harbor, since the caller reaches Harbor |

The sender never publishes to its own store for a peer (the former D4): that
would require the receiver to reach the sender's store and pass its SSRF policy,
which the receiver-driven model avoids. Three asymmetries drive the choice:

1. The control connection direction is not the data direction; A calling B does
   not mean B can reach A's store (the B-C-partitioned case).
2. Reachability is not symmetric, but a shared store is a common hub, so D2 is
   the robust default under partial mesh.
3. The caller always reaches Harbor (it is already calling Harbor's A2A
   endpoint), so Harbor can broker its own ingest (D3) without a shared store.

Harbor's default is **D2**, with **D3** as an optional mode. D1 is the bounded
fallback.

### 7.2 The storage extension

The structured contract goes in `Capabilities.Extensions[].Params`, which is the
only card field that carries machine-readable parameters (`skills` has no such
field). The URI mirrors the existing interaction extension. `ingest.modes` lists
the supported ways to hand Harbor a file; a client picks one it can perform.

```json
{
  "uri": "https://github.com/jiaweisky/harbor/extensions/storage/v1",
  "description": "Large-file exchange over a shared storage backend",
  "params": {
    "ingest": {
      "schemes": ["https", "s3", "cos"],
      "mediaTypes": ["text/*", "application/json", "application/xml", "image/*", "video/*", "audio/*"],
      "maxInlineBytes": 1048576,
      "maxBytes": 268435456,
      "modes": [
        { "mode": "reference",
          "upload": { "base": "https://store.example.com/in/harbor/", "method": "PUT", "auth": "none", "key": "{sha256}" } },
        { "mode": "broker" }
      ]
    }
  }
}
```

Fields:

- `ingest.schemes`: the reference schemes Harbor accepts as input.
- `ingest.mediaTypes`: the allowed ingest media types (the `media_types` policy
  above). A sender should only send a file whose type is listed.
- `ingest.maxInlineBytes`: the largest inline content Harbor accepts (inline
  below this size is always accepted); a sender should reference anything larger.
- `ingest.maxBytes`: the largest accepted reference.
- `ingest.modes`: the ingest capabilities Harbor supports. Each mode is:
  - `reference`: a static target is advertised (`upload.base`, `upload.method`,
    `upload.auth`, and an optional `upload.key` rule such as `{sha256}`). With no
    handshake, the sender forms the URL (`base + key`), uploads directly, and
    sends that URL in the `FilePart`. `upload` is omitted when the sender is
    expected to use the deployment's shared store (D2).
  - `broker`: the sender first obtains a per-file target from Harbor by sending a
    `storage/upload-slot-request` DataPart and reading the `storage/upload-slot`
    DataPart from the reply (section 7.5), then uploads and sends the returned
    reference.

The `ingest` limits are mandatory: they are the receiver's constraints, and a
sender learns from them what it may send (accepted inline size, reference size,
media types, and modes) rather than by having an artifact rejected. The current
inline caps (8 MiB image, 1 MiB file, 256 KB diff; section 1.1) were never
advertised; here they become `ingest.maxInlineBytes`. The sender's own inline
threshold (`inline_max_bytes`) is local and is not part of the contract.

The extension is declarative and optional (`required: false`). A client signals
the extensions it supports through the A2A `A2A-Extensions` header, and the server
reads the requested URIs; activation means the client understands this contract,
not that it can receive references. To receive large files from Harbor a client
supplies its own `ingest` contract (section 3). A `required` extension would force
every caller to support it, so it stays false.

The `reference` target can be hosted two ways:

- **Shared store open-write prefix**: `upload.base` points at the deployment's
  bucket or directory. The sender PUTs `base + key` and sends
  `FilePart(url = base + key)`; Harbor reads it back from the store.
- **Harbor's own ingest endpoint**: `upload.base` is
  `<agent base_url>/agents/<id>/storage/in/`. The sender PUTs directly to Harbor
  (reachable because it is already calling Harbor), then sends the URL; Harbor
  already holds the bytes and does not fetch at all.

`auth: none` means no credential is required to PUT, not that the ingest is
unbounded: the `media_types` and `maxBytes` policies and rate/quota limits still
apply to every uploaded file.

The schema is documented in `extensions/storage.md`, like `extensions/interaction.md`.

### 7.3 Skills supplement

`skills` carries no structured parameters, so it advertises the capability in
human/model terms only. Harbor adds one informational skill when storage is
configured; the machine contract stays in the extension.

```json
{
  "id": "file-exchange",
  "name": "File exchange",
  "description": "Exchanges large files through a shared storage backend. Accepts https/s3/cos references and inline content; broker mode available for presigned uploads.",
  "tags": ["files", "storage"],
  "inputModes":  ["text/plain", "application/json", "application/xml", "image/png", "image/jpeg"],
  "outputModes": ["text/plain", "application/json", "application/xml", "image/png", "image/jpeg"]
}
```

Division of labor: `skills` says "file exchange is supported", the default
modes say which formats are accepted, and the extension `params` say where the
store is and how to access it.

### 7.4 BuildCard generation

`bridge.BuildCard` currently derives extensions from capabilities
(`bridge/card.go` `interactionExtensions`). It gains the storage contract and
adds:

- `storageExtensions(contract)`: one `a2a.AgentExtension` with the URI and
  `Params` above, or nil when storage is unconfigured. The reference target comes
  from `Store.Target()`; broker support from `Store.Reserve`.
- one informational skill appended to the operator-configured skills, only when
  storage is configured.

The contract is assembled once at startup from `cfg.Storage` and the resolved
`blob.Store` (section 15).

### 7.5 Obtaining an upload target (broker mode)

In broker mode the sender cannot write to the store directly, so Harbor issues a
per-file presigned target over A2A. The extension defines two DataPart kinds; a
client that understands the extension knows their exact shape.

1. Reserve. The sender sends the agent a message whose content is a
   `storage/upload-slot-request` DataPart (it carries no prompt for the CLI):

   ```json
   { "kind": "storage/upload-slot-request",
     "name": "report.pdf", "size": 1048576,
     "mediaType": "application/pdf", "sha256": "..." }
   ```

   Harbor validates the request (size <= `maxBytes`, `mediaType` matched against
   `mediaTypes`, quota), rejects an unsupported request with
   `unsupported_content`, calls `Store.Reserve` (which signs a PUT into its own
   store), and completes the task with an artifact carrying a
   `storage/upload-slot` DataPart:

   ```json
   { "kind": "storage/upload-slot",
     "uploadUrl": "https://store.example.com/...presigned-put...",
     "method": "PUT",
     "headers": { "Content-Type": "application/pdf" },
     "reference": "s3://bucket/harbor/ab/abcdef.../report.pdf",
     "expiresAt": "2026-10-11T10:00:00Z" }
   ```

2. Upload. The sender PUTs the bytes to `uploadUrl` with `headers`.
3. Reference. The sender sends its task message with
   `FilePart(url=reference, filename, mediaType)`. `reference` is a URI in
   Harbor's own namespace, so Harbor reads it locally: no fetch, no SSRF, and the
   client does not need to understand the scheme.

The reserve is a normal A2A `message/send`, so it uses the same transport, bearer
auth, and card discovery as every other call; the agent handles it before the CLI
path and never starts a CLI. It is a request separate from the task because the
sender must hold the target before it can compose the message that references the
file. An HTTP variant exposing the same request/response under the agent's base
path is possible, but the A2A-message form keeps a single interface and the
existing auth.

### 7.6 Clients without the extension

A client that does not understand the extension still exchanges files:

- Inbound references it cannot honor are rejected with `unsupported_content`.
- It sends files inline (D1), bounded by `inline_max_bytes`.
- It never needs the model to reason about storage: it inlines files rather than
  negotiating a channel.

### 7.7 AgentCard examples by backend

The storage extension's `params` is derived from the configured backend; the
other card fields are the same. Below is a full card for the `http` backend, then
the differing `params` for `s3`, `cos`, and `local`. Fields not shown are
abbreviated, and only the storage extension is shown: other extensions (for
example the interaction extension) reflect the agent's effective capabilities and
are orthogonal to this design.

**`http`** (public upload base; broker via `presign_secret`):

```json
{
  "name": "Harbor/codebuddy",
  "description": "Remote A2A wrapper for the codebuddy coding agent CLI",
  "version": "2.161.4",
  "supportedInterfaces": [
    { "url": "https://harbor.example.com/agents/codebuddy/",      "protocolBinding": "JSONRPC",   "protocolVersion": "1.0" },
    { "url": "https://harbor.example.com/agents/codebuddy/rest/", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0" }
  ],
  "capabilities": {
    "streaming": true,
    "pushNotifications": true,
    "extensions": [
      { "uri": "https://github.com/jiaweisky/harbor/extensions/storage/v1",
        "description": "Large-file exchange over a shared storage backend",
        "params": {
          "ingest": {
            "schemes": ["https"],
            "mediaTypes": ["text/*", "application/json", "application/xml", "image/*"],
            "maxInlineBytes": 1048576,
            "maxBytes": 268435456,
            "modes": [
              { "mode": "reference",
                "upload": { "base": "https://store.example.com/in/harbor/", "method": "PUT", "auth": "none", "key": "{sha256}" } },
              { "mode": "broker" }
            ]
          }
        }
      }
    ]
  },
  "defaultInputModes":  ["text/plain", "image/png", "image/jpeg", "application/octet-stream"],
  "defaultOutputModes": ["text/plain", "application/json", "text/x-diff", "image/png"],
  "skills": [
    { "id": "file-exchange", "name": "File exchange",
      "description": "Exchanges large files through a shared storage backend. Accepts https/s3/cos references and inline content; broker mode available for presigned uploads.",
      "tags": ["files", "storage"],
      "inputModes":  ["text/plain", "application/json", "application/xml", "image/png", "image/jpeg"],
      "outputModes": ["text/plain", "application/json", "application/xml", "image/png", "image/jpeg"] }
  ]
}
```

**`s3`** (presigned broker; a native `s3://` reference target is advertised only
within a shared trust domain):

```json
"params": {
  "ingest": {
    "schemes": ["s3"],
    "mediaTypes": ["text/*", "application/json", "application/xml", "image/*"],
    "maxInlineBytes": 1048576,
    "maxBytes": 268435456,
    "modes": [ { "mode": "broker" } ]
  }
}
```

**`cos`** (same as `s3`, native `cos://`):

```json
"params": {
  "ingest": {
    "schemes": ["cos"],
    "mediaTypes": ["text/*", "application/json", "application/xml", "image/*"],
    "maxInlineBytes": 1048576,
    "maxBytes": 268435456,
    "modes": [ { "mode": "broker" } ]
  }
}
```

**`local`** (same-host or a shared mount; a remote peer cannot use `file://`):

```json
"params": {
  "ingest": {
    "schemes": ["file"],
    "mediaTypes": ["text/*", "application/json", "application/xml", "image/*"],
    "maxInlineBytes": 1048576,
    "maxBytes": 268435456,
    "modes": [
      { "mode": "reference",
        "upload": { "base": "file:///shared/harbor/in/", "method": "PUT", "auth": "none", "key": "{sha256}" } }
    ]
  }
}
```

For `local` with a `serve_base_url`, `upload.base` is that HTTP URL instead, so a
peer without the mount can upload.

## 8. Sender side (outbound artifacts)

Rework `emitChanges`, `emitImageArtifacts`, `emitFileArtifacts`, and
`emitChangedFile` in `internal/bridge/executor.go`.

Decision rule: inline when `size <= min(inline_max_bytes, consumer.maxInlineBytes)`
(the sender's local default and the consumer's `ingest.maxInlineBytes`), emitting
`a2a.NewRawPart`; otherwise upload to the consumer's advertised `ingest` target
and emit `a2a.NewFileURLPart(uri, mediaType)`. A consumer that supplied no
`ingest` contract gets the baseline: the sender assumes nothing and inlines.

- Remove the 8 MiB and 1 MiB caps. `readChangedFile` currently drops oversized
  files; replace it with a streaming path that hands the open file to the
  uploader (the consumer's target), so large files never enter memory. Only when
  the consumer is Harbor's own ingest endpoint does this become `Store.Put`.
- Text diff: `changes.Capture` still builds the baseline, but a diff above the
  threshold is uploaded as a file artifact and referenced instead of truncated.
- Image artifacts: small images stay inline, large images are referenced; the
  inline-versus-reference choice is the same delivery decision as any other file,
  and whether the model sees pixels is the receiver's `inbound_media`
  (section 9.1). The `maxImageBytes` cap is removed.
- `FilePart` metadata carries `{"sha256":..., "size":...}` for consumer
  verification; the name is in `FilePart.Filename`. `Artifact.Metadata` keeps
  `{"path":...}`.
- Key and reference follow the consumer's contract: the `ingest.upload.key` rule
  (for example `{sha256}`) forms the URL in reference mode, or the broker returns
  the reference (section 7.5). Content addressing still deduplicates and makes
  verification possible.
- Upload failure: when the file is within the inline bound, fall back to inline;
  otherwise skip the single artifact and record `storage_error` in the metadata.
  The task still completes, so a transient storage fault does not fail the whole
  task.

## 9. Consumer side (inbound files)

Rework `promptFromParts` and `materializeFile` in `internal/bridge/prompt.go`.

### 9.1 Projection: `inbound_media`

`storage.inbound_media` decides how an attachment the caller puts in the message
reaches the local model:

- `spool` (default): Harbor obtains the attachment's bytes — from inline bytes,
  or by reading a reference from the store (`Get`) — writes them under
  `<workspace>/.a2a/attachments/<turnID>/<name>` (replacing the old
  `<workdir>/.harbor/uploads/`), and adds a prompt note pointing at that path; the
  model reads it with its own tools. Any media type allowed by `media_types` is
  accepted, so ingestion does not depend on the model being multimodal.
- `inline`: project the bytes as native parts: the generic form is a
  `data:<mediaType>;base64,...` URI, which the CLI adapter converts to its own
  native content block (for example an image block for stream-json). The media
  reaches the model directly, and the CLI's media planner omits a modality it
  cannot carry.

`spool` is the modality-independent default; `inline` suits models and CLIs that
consume media natively.

### 9.2 Unifying bytes and references

| Inbound part | Today | Target |
|---|---|---|
| `FilePart(raw)` | written to uploads | projected per `inbound_media` |
| `FilePart(url)` (reference, D2/D3) | URL written into the prompt text | read via the store, then projected per `inbound_media` |

- Under `spool`, when the CLI accepts attachments by argument, the spooled path
  goes through `Request.Files`/`Request.ImageFiles` as today
  (`internal/integration` `fileArgs`/`imageArgs`); otherwise the path is named in
  the prompt note. Under `inline` there is no path: the media is projected as a
  native part.
- A reference is read from the configured store (`Get`) before projection; the
  model cannot dereference a native `s3://`, `cos://`, or `file://` itself, and
  Harbor does not fetch a peer-supplied URL. D2/D3 are the supported paths.
- Media type: the effective type (declared `FilePart.MediaType`, else inferred
  from `FilePart.Filename`) must match `media_types`; an unsupported type is
  rejected with `unsupported_content`. This applies to raw parts, references,
  and the broker reserve alike.
- Fail fast: a read or verification failure returns `unsupported_content` before
  the CLI starts, matching the existing up-front validation in `prompt.go`
  (`errUnsupportedContent`).

## 10. Context hygiene

- Handles only: outbound emits a reference (inline only for small files), and
  inbound `spool` writes a path. The model sees where the file is, not what it
  contains.
- `inbound_media: inline` is the deliberate exception: known media is projected
  as native parts (vision/audio) so the model sees the pixels or waveform;
  `ingest.maxInlineBytes` bounds the size accepted.
- `DataPart` handling is unchanged (section 2).

## 11. Security

- Store read, not arbitrary fetch: Harbor reads only its configured store's
  origin, which is trusted and may be private (nginx, MinIO, a COS VPC endpoint);
  it is not gated by an SSRF check. There is no peer-supplied URL to fetch, so the
  receiver-driven model has no SSRF surface beyond the store. (The push-webhook
  guard `push_allow_private_networks` is unrelated.)
- Limits: `download.max_bytes`, `download.timeout`, and a redirect cap; an
  exceedance returns `unsupported_content`.
- Oversized payloads: add an optional `server.max_request_bytes` to reject
  oversized JSON-RPC bodies before parsing, so inline bytes are not read into
  memory first.
- Secret sourcing: credentials and signing secrets come from environment
  interpolation (section 5), so the config file holds no plaintext secrets.
- Credential redaction: tokens and secrets never appear in logs, artifacts, or
  error messages, matching the existing redaction policy.
- Integrity: after reading a reference, verify `Part.Metadata.sha256` and reject a
  mismatch.
- `file://` confinement: an inbound `file://` reference is an arbitrary local
  file read unless confined. Only the `local` backend accepts `file://`; its
  `Get` is confined to the configured `root` (no traversal, no absolute path
  outside it, no key from another context or task). Every other backend returns
  `unsupported_content` for `file://`.
- A2A authentication and storage authentication are two separate token systems
  and are never conflated.

## 12. Lifecycle

- Content-addressed keys deduplicate: uploading the same file twice does not
  create a second object.
- Object cleanup is primarily the store's responsibility (for example an S3
  lifecycle rule). When a backend lacks one (`http`, `local`), Harbor may track
  objects and reap them by TTL; this is added only when needed.
- A broker's presigned upload target is signed at reserve time; its TTL covers
  the sender's upload window.

## 13. Degradation and compatibility

- No `storage` configured: `Store == nil`, no storage extension on the card, and
  the existing behavior is preserved exactly (inline, bounded, truncated).
  Existing deployments need no change.
- Storage configured: outbound artifacts above the threshold are uploaded to the
  consumer's `ingest` target and referenced (inlined when the consumer advertises
  no `ingest`); inbound references are read from the store; everything else is
  unchanged.
- A receiver whose card describes no file transfer: the sender assumes no file
  capability and uses the A2A baseline (inline). The receiver may support another
  means that is simply not advertised; the sender does not assume it cannot.

## 14. Observability and error codes

- Metrics: `harbor_blob_get_total{backend,result}` (store reads),
  `harbor_blob_upload_total{scheme,result}` (sender uploads to a target),
  `harbor_blob_reserve_total{result}` (broker slots),
  `harbor_blob_put_total{backend,result}` (own ingest endpoint), and a duration
  histogram.
- Logs: key, size, and result for each upload and read; failure reason
  (redacted).
- Error codes: reuse the existing task-status `errorCode` domain; add
  `storage_error` for upload/read failures. An unhandled URI remains
  `unsupported_content`.

## 15. Wiring

- `internal/server/server.go` `addAgent` builds one `blob.Store` from
  `cfg.Storage` before constructing executors, and passes it to
  `bridge.NewCLIExecutor`. The store is read-shared across agents.
- `CLIExecutor` gains a `store blob.Store` field, used by `toRequest` and
  `emitChanges`. Its `Execute` dispatches a `storage/upload-slot-request` message
  to the reserve handler before the CLI path, like `answerWithoutRunning`, so a
  reserve never starts a CLI.
- `bridge.BuildCard` gains the storage contract so it can emit the extension and
  the informational skill (section 7.4).
- New package `internal/blob`: `Store`, `UploadTarget`, `ErrUnsupportedURI`, the
  `http`, `s3`, `cos`, and `local` implementations, a scheme-keyed uploader for
  the sender side, and a `NewStorage(cfg) (Store, error)` factory.

## 16. Testing

- Unit: `local` Put-to-Get round trip; `http` backend against `httptest`
  fake PUT/GET; content-addressed keys and deduplication; `inline_max_bytes`
  boundaries (equal and greater).
- Bridge: an inbound `FilePart(url)` is read via a fake store and attached
  through `file_args` (in the style of the existing opencode attachment test);
  an outbound large file produces `FilePart(url)` rather than `Raw`.
- Projection: `spool` writes a path note and `inline` emits a native part, for
  both raw bytes and a store reference.
- Card: with storage configured, the card carries the storage extension with the
  expected `params` and the informational skill; with no storage, neither exists.
- Broker: a `storage/upload-slot-request` yields a `storage/upload-slot` DataPart
  with a `reference` the same store accepts; a request whose size or media type
  is out of policy is rejected with `unsupported_content`.
- Security: exceeding `max_bytes` is rejected; a sha256 mismatch is rejected; an
  unlisted media type is rejected with `unsupported_content`; a `file://`
  reference outside `root` is rejected.
- COS: the signature matches the reference vectors (the docs' SignKey /
  HttpString / StringToSign / Signature examples), a presigned GET carries the
  `q-sign-*` query parameters, and a presigned PUT round-trips.
- S3: the SigV4 signature matches the AWS reference examples (canonical request,
  string to sign, signing key, signature), a presigned GET carries the `X-Amz-*`
  query parameters, and both virtual-hosted and path-style round-trip.
- Self-hosted HTTP: a `PUT` to the write base then a `GET` from the read base
  round-trips; a `secure_link` signed URL verifies and an expired one is
  rejected.
- Degradation: the existing end-to-end tests stay green with no storage
  configured.
- Errors: `ErrUnsupportedURI` maps to `unsupported_content`.

## 17. Rollout

Each step is triggered by a current need; nothing is built ahead of demand.

1. `internal/blob`: interface, `NewStorage`, the `local` backend, and the config
   block.
2. AgentCard: advertise the storage extension (the `ingest` contract) and the
   informational skill.
3. Sender: artifacts above the threshold upload to the consumer's `ingest` target
   and emit `FileURLPart`; the 8 MiB and 1 MiB caps are removed.
4. Consumer: read `FilePart(url)` from the store; limits and sha256 verification.
5. `http` backend (public and presigned), the recommended default for
   cross-organization exchange.
6. `s3` backend (S3, OSS, MinIO; one API).
7. `cos` backend (Tencent COS native REST API; no S3 SDK).

## 18. Known limitations and future triggers

- Only the receiver-driven model is supported: to use out-of-band transfer, a
  receiver must advertise an `ingest` contract. When none is advertised the
  sender assumes no file capability and uses the A2A baseline (inline); it does
  not assume the receiver is incapable.
- Because A2A does not expose the caller's card, a caller that wants response
  files must include its own `ingest` contract (or card URL) in the request.
- A sender must be able to upload to the advertised target; a target in a scheme
  it cannot handle is rejected rather than silently dropped.
- `local` is same-host or NFS only; `file://` is not reachable across hosts.
- The consumer does no scheme dispatch; an unhandled URI fails fast rather than
  silently.
- No Range or media streaming: a large video is materialized whole. Added when a
  concrete media case appears.
- D3 (brokered presigned upload) is specified but not built until the concrete
  case "a peer cannot reach the shared store and can only reach Harbor" appears.
- Multipart upload is not implemented; COS and S3 presigned/simple uploads cap
  at about 5 GiB, so a larger transfer needs multipart (and, in broker mode, a
  negotiated part flow).
- A broker's presigned upload target is bearer-like: anyone holding it can upload
  until it expires. Access control is the store's, not Harbor's.
