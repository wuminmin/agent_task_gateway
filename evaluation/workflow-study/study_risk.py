#!/usr/bin/env python3
"""Recompute registered semantic-risk and cross-policy disclosure measures."""

from __future__ import annotations

import json
from pathlib import Path


HERE = Path(__file__).resolve().parent


def _load(name: str) -> dict:
    return json.loads((HERE / name).read_text(encoding="utf-8"))


def measure(facts: list[dict], task: dict) -> dict:
    sensitivity = _load("sensitivity-map.json")
    essential_doc = _load("essential-columns.json")
    essential_tasks = {**essential_doc["tasks"], **essential_doc.get("calibration_tasks", {})}
    essentials = essential_tasks.get(task["id"], task["approved_columns"])
    weights = sensitivity["weights"]
    namespaces = sensitivity["namespaces"]
    sensitive_records: set[str] = set()
    sensitive_fields: set[str] = set()
    unnecessary: set[str] = set()
    released_records: set[str] = set()
    released_fields: set[str] = set()
    released_cells: set[str] = set()
    released_values: set[str] = set()
    outcome_propositions: set[str] = set()
    negative_propositions: set[str] = set()
    weighted = 0
    for row in facts:
        identity = row["identity"]
        kind = identity.get("kind")
        namespace = identity.get("source_namespace", "")
        # Unknown namespaces fail closed at the high-sensitivity level.
        definition = namespaces.get(namespace, {"default": "high", "fields": {}})
        product = definition.get("product", "")
        level = definition.get("default", task["sensitivity"])
        if kind == "base-cell":
            level = definition.get("fields", {}).get(identity.get("field"), level)
        elif kind == "derived":
            levels = []
            for item in identity.get("snapshot_bundle", []):
                derived_definition = namespaces.get(
                    item.get("source_namespace", ""), {"default": "high", "fields": {}},
                )
                # A derived Fact carries relation snapshots but not field-level
                # lineage. Charge the most sensitive configured field in each
                # source relation so aggregation cannot silently downgrade a
                # high-sensitivity input to its namespace default.
                candidate_levels = [
                    derived_definition.get("default", "high"),
                    *derived_definition.get("fields", {}).values(),
                ]
                levels.append(max(candidate_levels, key=lambda item: weights[item]))
            level = max(levels, key=lambda item: weights[item]) if levels else task["sensitivity"]
        elif kind == "outcome":
            level = task["sensitivity"]
        weight = weights[level]
        weighted += weight
        if weight > 1 and identity.get("entity_key"):
            sensitive_records.add(namespace + "\0" + identity["entity_key"])
        if weight > 1 and identity.get("field"):
            field_key = namespace + "\0" + identity["field"]
            sensitive_fields.add(field_key)
            if identity["field"] not in essentials.get(product, []):
                unnecessary.add(field_key)
        if row["ledger_kind"] == "RELEASE" and weight > 1:
            if identity.get("entity_key"):
                released_records.add(namespace + "\0" + identity["entity_key"])
            if identity.get("field"):
                released_fields.add(namespace + "\0" + identity["field"])
            if kind == "base-cell" and identity.get("entity_key") and identity.get("field"):
                released_cells.add(namespace + "\0" + identity["entity_key"] + "\0" + identity["field"])
            if identity.get("canonical_value"):
                released_values.add(
                    namespace + "\0" + str(identity.get("field", "")) + "\0" + identity["canonical_value"]
                )
        if row["ledger_kind"] == "OUTCOME" and identity.get("query_normal_form_sha256"):
            outcome_propositions.add(identity["query_normal_form_sha256"])
            # Go omits outcome_rows for the zero value, so a missing field is a
            # registered zero-row outcome rather than an unknown value.
            if int(identity.get("outcome_rows", 0)) == 0:
                negative_propositions.add(identity["query_normal_form_sha256"])
    counts = {
        ledger_kind: sum(row["ledger_kind"] == ledger_kind for row in facts)
        for ledger_kind in ("RELEASE", "INFLUENCE", "OUTCOME")
    }
    return {
        "release_facts": counts["RELEASE"],
        "influence_facts": counts["INFLUENCE"],
        "outcome_facts": counts["OUTCOME"],
        "sensitivity_weighted_exposure": weighted,
        "distinct_sensitive_records": len(sensitive_records),
        "distinct_sensitive_fields": len(sensitive_fields),
        "unnecessary_sensitive_fields": len(unnecessary),
        "released_sensitive_records": len(released_records),
        "released_sensitive_fields": len(released_fields),
        "released_sensitive_cells": len(released_cells),
        "released_sensitive_values": len(released_values),
        "disclosed_outcome_propositions": len(outcome_propositions),
        "disclosed_negative_propositions": len(negative_propositions),
    }
