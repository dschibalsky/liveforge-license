"""
Reference contract for the LiveForge license HTTP API.

**Implementation in this repo:** `backend/main.go` (Go) — routes `POST /api/keys/verify`
and `POST /api/keys/consume`. The Next.js app calls the verify URL from
`gui/app/lib/license-backend-client.ts` (`LIVEFORGE_LICENSE_BACKEND_VERIFY_URL` or
`LIVEFORGE_LICENSE_BACKEND_BASE_URL` + `/api/keys/verify`) with JSON body::

    { "key": "<LICENSE>", "machine_id": "<MACHINE_UUID>" }

`key` is matched case-insensitively (client may send uppercase). When
`machine_id` is non-empty, the backend activates or refreshes a device slot
(`last_seen_at`) and enforces `max_devices`.

**Successful verify response** (HTTP 200)::

    {
      "ok": true,
      "license": { ... },
      "validated_at": "<RFC3339>"
    }

`license` uses **snake_case** fields below (camelCase is *not* emitted by the Go
backend; the TS client accepts both when parsing). Extra fields (e.g.
`user_id`) are ignored by the desktop normalizer.

**Hosted-only fields on `license`:** when `billing_mode` is `hosted`, the
backend includes `wallet_balance_usd` and `hosted_tier_presets` (t1–t3). That
balance is the **server-side allowance** for debits and for the in-app usage
meter math; the cloud GUI does **not** show it as a cash wallet to the user.

**Wallet debits vs. OpenRouter**

Hosted chat can POST idempotent debits to `POST /api/keys/consume` (configure
`LIVEFORGE_LICENSE_BACKEND_CONSUME_URL` in the Next.js host). Payload (JSON)::

    {
      "license_key": "<LICENSE>",
      "machine_id": "<MACHINE_UUID>",
      "trace_id": "<UNIQUE_PER_TURN>",
      "session_id": "<optional>",
      "provider_cost_usd": <float>,
      "wallet_debit_usd": <float>,
      "estimated": <bool>,
      "billing_mode": "hosted"
    }

When `CONSUME_SECRET` is set on the Go server, the client must send header
`x-liveforge-consume-secret: <same value>` (`LIVEFORGE_LICENSE_BACKEND_CONSUME_SECRET`).

**Consume responses:** `200` with `ok: true` and optionally `license` (updated
wallet + presets). Duplicate `trace_id` returns `200` with `idempotent: true`.
Common errors: `402` + `insufficient_balance`, `403` + `machine_not_activated`,
`403` + `invalid_key`, `400` + `billing_mode_must_be_hosted`.

OpenRouter’s customer “credits” API is **not** the billing source of truth for
LiveForge packages — that remains this backend + hosted keys.

**Shop links (GUI):** optional `NEXT_PUBLIC_LIVEFORGE_TOPUP_URL_15`, `_30`,
`_60`, or a single `NEXT_PUBLIC_LIVEFORGE_TOPUP_URL` — the app may show three
neutral pack buttons appending `liveforge_pack=15|30|60` for your storefront.
"""

from __future__ import annotations

from typing import Literal, NotRequired, TypedDict

HostedTierId = Literal["t1", "t2", "t3"]


class HostedTierTriple(TypedDict):
    model: str
    compose_model: str
    utility_model: str


class HostedTierPresets(TypedDict, total=False):
    t1: HostedTierTriple
    t2: HostedTierTriple
    t3: HostedTierTriple


class LicenseVerifyLicense(TypedDict, total=False):
    key_last4: str
    user_id: str
    status: str
    max_devices: int
    active_devices: int
    expires_at: str | None
    billing_mode: Literal["byok", "hosted"]
    allowed_edition: Literal["full", "cloud"]
    wallet_balance_usd: float
    model_tier: HostedTierId
    default_hosted_tier: HostedTierId
    hosted_tier_presets: HostedTierPresets
    plan: str


class LicenseVerifyOk(TypedDict):
    ok: Literal[True]
    license: LicenseVerifyLicense
    validated_at: str


class WalletConsumeRequest(TypedDict, total=False):
    license_key: str
    machine_id: str
    trace_id: str
    session_id: str
    provider_cost_usd: float
    wallet_debit_usd: float
    estimated: bool
    billing_mode: Literal["hosted"]
