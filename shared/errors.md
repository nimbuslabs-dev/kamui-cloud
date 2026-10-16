# Errors

Every service returns errors in this one shape, with the right HTTP status.

```json
{"error": {"code": "invalid_path", "message": "key must not contain .."}}
```

## Codes

| HTTP | `code` | When to use it |
|---|---|---|
| 400 | `bad_request` | Body is not valid JSON, a field is missing or has the wrong type |
| 400 | `invalid_path` | Bucket or key is invalid, contains `..`, a slash or starts with a dot |
| 401 | `unauthorized` | Token is missing or wrong |
| 404 | `not_found` | Node, job, bucket or object does not exist |
| 409 | `conflict` | Action not allowed now, for example finishing a job that is not running |
| 413 | `payload_too_large` | Upload is larger than `KAMUI_MAX_MB` |
| 503 | `unavailable` | Service is up but not ready, for example the model is not loaded |
| 500 | `internal` | Unexpected bug. Log it, do not leak details in the message |

## Rules

- Success is 200 with a body, 201 for created, 204 with no body.
- Only `/healthz` works without a token.
- Do not put tokens, file contents or stack traces in `message`.
- Agents treat a 404 on heartbeat as "I was removed" and register again.
- Clients retry only on network errors and 503, never on 4xx.
