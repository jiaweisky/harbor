# Harbor interaction extension (v1)

URI: `https://github.com/jiaweisky/harbor/extensions/interaction/v1`

When a coding-agent CLI pauses for client input, Harbor moves the task to
`input-required` and attaches a message whose parts are:

- a `text/plain` `TextPart` with a human-readable prompt, and
- an `application/json` `DataPart` carrying the structured interaction below.

A client that understands the extension answers with a `DataPart`; a client that
does not can answer with plain text and Harbor treats the text as the answer.

## Interaction (server → client)

```json
{
  "kind": "permission",
  "id": "itx_9f2a1c40b7d3e511",
  "action": "execute",
  "target": "run tests",
  "risk": "high",
  "options": ["allow_once", "reject_once"],
  "resume": true
}
```

| field | type | required | meaning |
|---|---|---|---|
| `kind` | `"permission" \| "question" \| "plan_approval"` | yes | Why input is needed. |
| `id` | string | yes | Interaction id; echo it back so Harbor can reject cross-talk. |
| `action` | string | no | Short verb, e.g. `execute`, `write`. |
| `target` | string | no | What the action applies to (command, path, question text). |
| `risk` | string | no | Free-form risk hint (`low`/`medium`/`high`) when known. |
| `options` | string[] | no | Allowed answers. Empty means free text. |
| `questions` | object[] | no | Every question of a multi-question ask (kind `question`, action `question`); each has `question`, optional `header`, and `options`. A single question is also mirrored in `target`/`options`. |
| `schema` | object | no | Form JSON Schema (kind `question`, action `form`): the fields to fill. |
| `resume` | bool | yes | True when the original process is kept alive and resumed. |

The interaction id is also exposed as `harbor.interactionId` in the message
metadata.

## Answer (client → server)

```json
{ "kind": "permission", "id": "itx_9f2a1c40b7d3e511", "answer": "allow_once" }
```

A form (`action: "form"`) is answered with `content` keyed by the schema's fields:

```json
{ "id": "itx_9f2a1c40b7d3e511", "content": { "name": "Ada", "favorite_color": "blue" } }
```

A multi-question ask (kind `question` with a non-empty `questions`) is answered
the same way: `content` keyed by each question's text.

```json
{ "id": "itx_9f2a1c40b7d3e511", "content": { "Favorite color?": "Red", "Favorite animal?": "Dog" } }
```

| field | type | required | meaning |
|---|---|---|---|
| `kind` | string | no | Echo of the request kind. |
| `id` | string | no | The interaction id being answered. Required to be validated when present. |
| `answer` | string | yes* | An `options` value, or free text for a `question` (*omit for a form, which uses `content`). |
| `content` | object | yes* | Form answer keyed by the schema's field names (*required for `action:"form"`). |

If `id` is present and does not match the pending interaction, Harbor fails the
task with `harbor.errorCode = "internal"` instead of resuming with a stale
answer. Omit `id` only for text-only clients. A text-only client may still
echo the id in the message metadata as `harbor.interactionId`.

## JSON Schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/jiaweisky/harbor/extensions/interaction/v1",
  "definitions": {
    "interaction": {
      "type": "object",
      "required": ["kind", "id", "resume"],
      "properties": {
        "kind": { "enum": ["permission", "question", "plan_approval"] },
        "id": { "type": "string" },
        "action": { "type": "string" },
        "target": { "type": "string" },
        "risk": { "type": "string" },
        "options": { "type": "array", "items": { "type": "string" } },
        "questions": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["question"],
            "properties": {
              "question": { "type": "string" },
              "header": { "type": "string" },
              "options": { "type": "array", "items": { "type": "string" } }
            }
          }
        },
        "schema": { "type": "object" },
        "resume": { "type": "boolean" }
      }
    },
    "answer": {
      "type": "object",
      "properties": {
        "kind": { "type": "string" },
        "id": { "type": "string" },
        "answer": { "type": "string" },
        "content": { "type": "object" }
      }
    }
  }
}
```
