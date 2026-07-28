#!/usr/bin/env python3
"""Buffer-before-release controllers for the three baseline policy arms."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass


BASELINE_ARMS = {"query_count", "returned_rows", "serialized_bytes"}


@dataclass(frozen=True)
class Admission:
    released: bool
    charged: int
    used: int
    remaining: int
    idempotent_retry: bool = False


class BaselineController:
    """Enforce one baseline budget without releasing an over-budget payload.

    The caller executes a query into a private buffer, computes its visible row
    count, and passes the exact canonical response bytes here. A request ID is
    transport-idempotent: retrying the same buffered result repeats the prior
    decision without charge, while reusing it for a different result is an
    error. Semantic replay under a new request ID is charged again.
    """

    def __init__(self, arm: str, ceiling: int) -> None:
        if arm not in BASELINE_ARMS:
            raise ValueError(f"unsupported baseline arm: {arm}")
        if not isinstance(ceiling, int) or isinstance(ceiling, bool) or ceiling < 0:
            raise ValueError("ceiling must be a non-negative integer")
        self.arm = arm
        self.ceiling = ceiling
        self.used = 0
        self.runtime_budget_rejections = 0
        self._requests: dict[str, tuple[str, Admission]] = {}

    def admit(self, request_id: str, visible_rows: int, serialized_payload: bytes) -> Admission:
        if not request_id:
            raise ValueError("request_id is required")
        if not isinstance(visible_rows, int) or isinstance(visible_rows, bool) or visible_rows < 0:
            raise ValueError("visible_rows must be a non-negative integer")
        if not isinstance(serialized_payload, bytes):
            raise TypeError("serialized_payload must be the buffered bytes")

        fingerprint = hashlib.sha256(
            str(visible_rows).encode("ascii") + b"\x00" + serialized_payload
        ).hexdigest()
        prior = self._requests.get(request_id)
        if prior is not None:
            prior_fingerprint, prior_admission = prior
            if prior_fingerprint != fingerprint:
                raise ValueError("request_id was reused for a different buffered result")
            return Admission(
                released=prior_admission.released,
                charged=0,
                used=self.used,
                remaining=self.ceiling - self.used,
                idempotent_retry=True,
            )

        charge = {
            "query_count": 1,
            "returned_rows": visible_rows,
            "serialized_bytes": len(serialized_payload),
        }[self.arm]
        if self.used + charge > self.ceiling:
            self.runtime_budget_rejections += 1
            admission = Admission(False, 0, self.used, self.ceiling - self.used)
        else:
            self.used += charge
            admission = Admission(True, charge, self.used, self.ceiling - self.used)
        self._requests[request_id] = (fingerprint, admission)
        return admission


def canonical_response_bytes(value: object) -> bytes:
    """Serialize exactly once for the byte arm and subsequent agent release."""

    return json.dumps(
        value,
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
        allow_nan=False,
    ).encode("utf-8")
