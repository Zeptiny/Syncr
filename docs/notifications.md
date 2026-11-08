# Notifications

Syncr notifies **contacts** when jobs finish and when scheduled runs are
missed or cannot start. A contact is one configured channel: a Discord
webhook, an email address list behind an SMTP server, or a generic webhook.
Contacts belong to the user who created them; their secrets (webhook URLs,
passwords, signing secrets) are encrypted at rest and never shown again.

What triggers a notification:

| Event | When |
|---|---|
| `succeeded` | A job finished successfully. |
| `failed` | A job failed (a scheduled job that was never dispatched included), or a scheduled run could not be launched (worker removed, definition no longer valid, no worker with the label). |
| `interrupted` | A job was interrupted (worker restart, shutdown or removal). |
| `missed` | A scheduled run did not start before its deadline (worker offline, overlap). |
| `test` | **Send test** on the contact page. |

Cancelled jobs never notify. A schedule links contacts with the events each
one gets (by default `failed`, `interrupted` and `missed`). A contact's
**Manual jobs** events apply to the jobs its owner starts by hand; they are
empty by default.

Deliveries are durable: they are planned in the database, sent by a
background worker and retried with backoff (30 s doubling up to 1 h, about
eight attempts over an hour, or the delay the target asks for with
`Retry-After`). Each contact page shows its delivery log, and a delivery that
gave up can be retried. Delivery is **at least once**: after a crash or a
timeout the same notification can arrive twice. Webhook receivers can
deduplicate on `webhook-id`, and emails carry a stable `Message-ID`.

## Network policy

Notification targets are user-supplied URLs and hosts, so the controller
checks every address it connects to, after DNS resolution and at the moment
it connects (a name that later resolves to `127.0.0.1` is still refused).
Redirects are never followed.

| `notify.network_policy` | Allows |
|---|---|
| `private` (default) | Everything except loopback, link-local (including the cloud metadata address `169.254.169.254`), other metadata addresses, unspecified, multicast and broadcast addresses. LAN targets such as Home Assistant or an internal SMTP relay work. |
| `public` | Only public addresses: also refuses RFC 1918, `fc00::/7`, `100.64.0.0/10` and other special-purpose ranges. |
| `any` | Every address (for tests and single-host setups). |

`notify.allowed_networks` lists CIDRs that are allowed whatever the policy,
for example `127.0.0.1/32` for a relay on the controller's host. A refused
address fails the delivery permanently with "the address is not allowed by
the notification network policy".

Proxies are not supported for notifications: `HTTPS_PROXY` and friends are
ignored, because the policy would then check the proxy instead of the target.

## Discord

1. In Discord, open the channel's **Edit Channel → Integrations → Webhooks**
   and create a webhook. Copy its URL.
2. In Syncr, open **Notifications → New contact → Discord** and paste the
   URL. Optionally set a bot name (Discord rejects names containing
   "discord" or "clyde").

The URL must be `https://discord.com/api/webhooks/<id>/<token>` (also
`discordapp.com`, `ptb.discord.com` and `canary.discord.com`; a
`?thread_id=` is allowed). Messages are embeds colored by event. Names are
markdown-escaped and mentions are disabled (`allowed_mentions` parses
nothing), so a schedule named `@everyone` pings no one.

A deleted webhook (404) or a bad token (401, 403) fails permanently; rate
limits (429) and Discord outages (5xx) are retried.

## Email

| Field | Notes |
|---|---|
| SMTP server, port | Port 587 for STARTTLS, 465 for implicit TLS, 25 for a local relay. |
| Security | `STARTTLS` (default): the server must offer STARTTLS, or the send fails. `TLS`: implicit TLS from the first byte. `None`: clear text, only for a trusted relay, and never with a password. |
| Username, password | PLAIN or LOGIN authentication (LOGIN is what Office 365 and some relays need). Credentials are only sent over TLS. |
| From | One address, e.g. `Syncr <syncr@example.com>`. |
| To | Up to 20 addresses, separated by commas. |
| EHLO name | Advanced; defaults to the controller's host name. |

The server's certificate is verified against the host name, with TLS 1.2 or
later. Each message is `multipart/alternative` (plain text and HTML) with
`Message-ID: <delivery-id@syncr>`, `Auto-Submitted: auto-generated` and
`X-Syncr-Event: <event>`, so mail rules can filter it. An SMTP 5xx reply
(unknown recipient, authentication failed) fails permanently; 4xx replies
and network errors are retried.

## Webhook

| Field | Notes |
|---|---|
| URL | `http` or `https`. Kept secret, since webhook URLs often contain a token. |
| Format | `Syncr JSON` (default, below) or `Slack-compatible` (`{"text": "..."}`, accepted by Slack, Mattermost and Rocket.Chat incoming webhooks). |
| Signing secret | Optional. `whsec_` followed by base64 (the Standard Webhooks format; the UI can generate one), or any string of at least 24 characters. |
| Authorization header | Advanced, optional; sent as is, e.g. `Bearer <token>`. |

Replies: any 2xx is success. 408, 425, 429 and 5xx are retried (honoring
`Retry-After`). Redirects and other 4xx fail permanently.

### Request

```http
POST /your/path HTTP/1.1
Content-Type: application/json
User-Agent: Syncr-Webhook/1
webhook-id: 0195f0a4-3b7c-7d1e-9f00-123456789abc
webhook-timestamp: 1793759467
webhook-signature: v1,<base64 HMAC-SHA256>
```

The headers follow the [Standard Webhooks](https://www.standardwebhooks.com)
specification:

- `webhook-id` is the delivery ID. It is the same on every retry of the
  delivery: use it to ignore duplicates.
- `webhook-timestamp` is the attempt's time in Unix seconds. Reject requests
  whose timestamp is far from your clock (the specification suggests five
  minutes) to stop replays.
- `webhook-signature` is present when a signing secret is set:
  `v1,` followed by the base64 HMAC-SHA256 of `<webhook-id>.<webhook-timestamp>.<body>`,
  keyed with the secret (for `whsec_` secrets, the base64-decoded bytes after
  the prefix; otherwise the secret's bytes). The header may list several
  space-separated signatures; accept the request if any one matches.

### Payload (format "Syncr JSON")

The body is one JSON object. Fields are only added within a `version`;
unknown fields should be ignored.

| Field | Type | Meaning |
|---|---|---|
| `version` | number | Payload version, currently `1`. |
| `id` | string | The delivery ID (equals `webhook-id`). |
| `event` | string | `succeeded`, `failed`, `interrupted`, `missed` or `test`. |
| `occurred_at` | RFC 3339 time | When it happened (the job finished, the run was missed). |
| `title` | string | One-line summary, at most 200 characters: the event and the schedule's name, or for a job without a schedule its description (e.g. `Job failed: Copy Photos → B2 offsite`). |
| `url` | string | Link to the job or schedule in the web UI; absent without `http.public_url`. |
| `schedule` | object | For scheduled jobs and runs: `id`, `name`. |
| `job` | object | For job outcomes (below). |
| `run` | object | For missed or failed runs (below). |
| `test` | boolean | `true` on test notifications; absent otherwise. |

`job`: `id`, `kind` (e.g. `rclone_transfer`), `description` (what the job
does, as the jobs list shows it, e.g. `Copy Photos → B2 offsite`: the mode,
then the source and destination by remote name or path on the worker;
absent if the job could not be described), `status`, `worker_id`,
`worker_name`, `error_code` and `error_message` (absent on success), `bytes`,
`files`, `errors`, `started_at` and `finished_at` (absent if the job never
started or finished), `duration_seconds`.

`run`: `id`, `scheduled_for` (the occurrence), `reason` (a code:
`worker_offline`, `overlap`, `controller_unavailable`, `remote_unavailable`,
`invalid_definition`, `worker_removed`, `no_worker_with_label`,
`owner_disabled`), `message` (details, may be absent), `coalesced` (earlier
occurrences merged into this run).

A failed scheduled job:

```json
{
  "version": 1,
  "id": "0195f0a4-3b7c-7d1e-9f00-123456789abc",
  "event": "failed",
  "occurred_at": "2026-11-04T02:31:07Z",
  "title": "Job failed: Nightly photos → B2",
  "url": "https://syncr.example/jobs/0195f0a4-3b7c-7d1e-9f00-000000000001",
  "schedule": {
    "id": "0195f0a4-3b7c-7d1e-9f00-000000000002",
    "name": "Nightly photos → B2"
  },
  "job": {
    "id": "0195f0a4-3b7c-7d1e-9f00-000000000001",
    "kind": "rclone_transfer",
    "description": "Copy Photos → B2 offsite",
    "status": "failed",
    "worker_id": "0195f0a4-3b7c-7d1e-9f00-000000000003",
    "worker_name": "nas-01",
    "error_code": "path_not_allowed",
    "error_message": "the source is outside the worker's local_roots",
    "bytes": 1048576,
    "files": 12,
    "errors": 1,
    "started_at": "2026-11-04T02:29:37Z",
    "finished_at": "2026-11-04T02:31:07Z",
    "duration_seconds": 90
  }
}
```

A missed run:

```json
{
  "version": 1,
  "id": "0195f0a4-3b7c-7d1e-9f00-123456789abd",
  "event": "missed",
  "occurred_at": "2026-11-04T02:45:00Z",
  "title": "Scheduled run missed: Office docs hourly",
  "url": "https://syncr.example/schedules/0195f0a4-3b7c-7d1e-9f00-000000000004",
  "schedule": {
    "id": "0195f0a4-3b7c-7d1e-9f00-000000000004",
    "name": "Office docs hourly"
  },
  "run": {
    "id": "0195f0a4-3b7c-7d1e-9f00-000000000005",
    "scheduled_for": "2026-11-04T02:00:00Z",
    "reason": "worker_offline",
    "message": "office-pc was offline until the deadline",
    "coalesced": 2
  }
}
```

### Verifying the signature

Any Standard Webhooks library works (`standardwebhooks` for Python,
JavaScript, Go and others). Without one, the check is a few lines.

Python:

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers: dict, body: bytes, tolerance: int = 300) -> bool:
    msg_id = headers["webhook-id"]
    timestamp = headers["webhook-timestamp"]
    if abs(time.time() - int(timestamp)) > tolerance:
        return False
    key = base64.b64decode(secret.removeprefix("whsec_")) if secret.startswith("whsec_") else secret.encode()
    signed = f"{msg_id}.{timestamp}.".encode() + body
    expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest()).decode()
    for sig in headers.get("webhook-signature", "").split():
        version, _, value = sig.partition(",")
        if version == "v1" and hmac.compare_digest(value, expected):
            return True
    return False
```

Go:

```go
func verify(secret string, h http.Header, body []byte) bool {
	key := []byte(secret)
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		var err error
		if key, err = base64.StdEncoding.DecodeString(rest); err != nil {
			return false
		}
	}
	ts, err := strconv.ParseInt(h.Get("webhook-timestamp"), 10, 64)
	if err != nil || math.Abs(time.Since(time.Unix(ts, 0)).Seconds()) > 300 {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(h.Get("webhook-id") + "." + h.Get("webhook-timestamp") + "."))
	mac.Write(body)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, sig := range strings.Fields(h.Get("webhook-signature")) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}
```

Verify against the raw body bytes as received, before any JSON parsing.
To test your verifier, use the specification's test vector: secret
`whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw`, id `msg_p5jXN8AQM9LWM0D4loKWxJek`,
timestamp `1614265330` and body `{"test": 2432232314}` must give
`v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=` (ignore the timestamp
tolerance for this check).

### Home Assistant example

Create an automation with a **Webhook** trigger (its URL looks like
`http://homeassistant.local:8123/api/webhook/<id>`), add it as a webhook
contact in Syncr with format `Syncr JSON`, and use
`{{ trigger.json.title }}` and `{{ trigger.json.event }}` in the actions.
Home Assistant does not check signatures; leave the signing secret empty or
verify it in a custom integration.
