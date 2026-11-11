# /// script
# requires-python = ">=3.11"
# dependencies = ["nats-py>=2.16,<3"]
# ///
"""Example extension: a daily token budget per tenant.

It talks to the gateway only through NATS:
  1. Reads usage records from the JetStream stream AIF_USAGE (from UTC midnight on start, then live).
  2. Answers the pre-request hook: deny with 429 when the tenant used up today's tokens.

Keys without a tenant_id are budgeted per key. Run several copies: they share hook traffic
through the queue group, and each one reads the full usage stream.

Environment:
  NATS_URL               default nats://127.0.0.1:4222
  BUDGET_DAILY_TOKENS    default limit per tenant per UTC day (default 100000)
  BUDGET_TENANT_LIMITS   per-tenant overrides, e.g. "acme=5000,beta=200000"

Run:
  uv run extensions/example-budget/budget.py
"""

import asyncio
import json
import logging
import os
import signal
from datetime import datetime, timezone

import nats
from nats.js.api import ConsumerConfig, DeliverPolicy

HOOK_SUBJECT = "aif.hook.pre_request.v1"
USAGE_SUBJECT = "aif.usage.v1.>"
USAGE_STREAM = "AIF_USAGE"
QUEUE = "example-budget"

log = logging.getLogger("budget")


def parse_limits(text: str) -> dict[str, int]:
    out = {}
    for part in filter(None, (p.strip() for p in text.split(","))):
        name, _, value = part.partition("=")
        out[name.strip()] = int(value)
    return out


def owner(tenant_id: str, key_id: str) -> str:
    return tenant_id or f"key:{key_id}"


class Budget:
    def __init__(self, default_limit: int, limits: dict[str, int]):
        self.default_limit = default_limit
        self.limits = limits
        self.day = self.today()
        self.used: dict[str, int] = {}

    @staticmethod
    def today() -> str:
        return datetime.now(timezone.utc).date().isoformat()

    def roll(self) -> None:
        if (d := self.today()) != self.day:
            self.day, self.used = d, {}

    def add(self, rec: dict) -> None:
        self.roll()
        if rec.get("created_at", "")[:10] != self.day:
            return  # replayed record from an earlier day
        who = owner(rec.get("tenant_id", ""), rec.get("key_id", ""))
        self.used[who] = self.used.get(who, 0) + int(rec.get("total_tokens") or 0)

    def decide(self, req: dict) -> dict:
        self.roll()
        key = req.get("key") or {}
        who = owner(key.get("tenant_id", ""), key.get("id", ""))
        limit = self.limits.get(who, self.default_limit)
        used = self.used.get(who, 0)
        if used >= limit:
            log.info("deny %s: %d/%d tokens today (request %s)", who, used, limit, req.get("request_id"))
            return {"action": "deny", "status": 429, "code": "budget_exceeded",
                    "message": f"daily token budget used up ({used}/{limit}); resets at 00:00 UTC"}
        return {"action": "allow"}


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    budget = Budget(int(os.environ.get("BUDGET_DAILY_TOKENS", "100000")),
                    parse_limits(os.environ.get("BUDGET_TENANT_LIMITS", "")))

    nc = await nats.connect(os.environ.get("NATS_URL", "nats://127.0.0.1:4222"), name="example-budget",
                            max_reconnect_attempts=-1)
    js = nc.jetstream()

    async def on_usage(msg):
        try:
            budget.add(json.loads(msg.data))
        except (ValueError, TypeError) as e:
            log.warning("bad usage record on %s: %s", msg.subject, e)

    midnight = datetime.now(timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
    # Ordered consumer: ephemeral, no acks, replays today's records and then follows live ones.
    await js.subscribe(USAGE_SUBJECT, stream=USAGE_STREAM, cb=on_usage, ordered_consumer=True,
                       config=ConsumerConfig(deliver_policy=DeliverPolicy.BY_START_TIME,
                                             opt_start_time=midnight.isoformat().replace("+00:00", "Z")))

    async def on_hook(msg):
        try:
            reply = budget.decide(json.loads(msg.data))
        except (ValueError, TypeError) as e:
            log.warning("bad hook request: %s", e)
            reply = {"action": "allow"}
        await msg.respond(json.dumps(reply).encode())

    await nc.subscribe(HOOK_SUBJECT, queue=QUEUE, cb=on_hook)
    log.info("running: default limit %d tokens/day, overrides %s", budget.default_limit, budget.limits or "none")

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()
    await nc.drain()


if __name__ == "__main__":
    asyncio.run(main())
