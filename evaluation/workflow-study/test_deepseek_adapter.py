#!/usr/bin/env python3

from __future__ import annotations

import unittest
from unittest import mock

import deepseek_agent_adapter as adapter


def execution_lock(*, max_attempts: int = 4) -> dict:
    return {
        "api_base_url": "https://api.deepseek.com",
        "model": "deepseek-v4-flash",
        "thinking_mode": "disabled",
        "temperature": 0,
        "top_p": 1.0,
        "max_tokens": 4096,
        "request_timeout_seconds": 300,
        "api_retry": {
            "max_attempts": max_attempts,
            "initial_backoff_seconds": 0.1,
            "max_backoff_seconds": 0.5,
            "retryable_http_statuses": [429, 500, 502, 503, 504],
            "retry_insufficient_system_resource": True,
        },
    }


def response(
    *,
    finish_reason: str = "stop",
    fingerprint: str = "fp-v4-test",
    prompt_tokens: int = 10,
    cache_hit_tokens: int = 4,
    completion_tokens: int = 3,
    reasoning_tokens: int = 0,
    content: str = "done",
) -> dict:
    return {
        "model": "deepseek-v4-flash",
        "system_fingerprint": fingerprint,
        "choices": [
            {
                "finish_reason": finish_reason,
                "message": {"role": "assistant", "content": content},
            }
        ],
        "usage": {
            "prompt_tokens": prompt_tokens,
            "prompt_cache_hit_tokens": cache_hit_tokens,
            "prompt_cache_miss_tokens": prompt_tokens - cache_hit_tokens,
            "completion_tokens": completion_tokens,
            "completion_tokens_details": {"reasoning_tokens": reasoning_tokens},
            "total_tokens": prompt_tokens + completion_tokens,
        },
    }


class DeepSeekV4AdapterTests(unittest.TestCase):
    def call(self, lock: dict | None = None) -> tuple[dict, str, dict]:
        return adapter.call_deepseek(
            [{"role": "user", "content": "offline fixture"}],
            [{"type": "function", "function": {"name": "noop", "parameters": {"type": "object"}}}],
            lock or execution_lock(),
            "offline-not-a-real-key",
        )

    def test_request_explicitly_disables_thinking(self) -> None:
        with mock.patch.object(adapter, "http_json", return_value=response()) as request:
            self.call()

        url, payload, headers = request.call_args.args
        self.assertEqual(url, "https://api.deepseek.com/chat/completions")
        self.assertEqual(payload["model"], "deepseek-v4-flash")
        self.assertEqual(payload["thinking"], {"type": "disabled"})
        self.assertEqual(payload["temperature"], 0)
        self.assertEqual(payload["top_p"], 1.0)
        self.assertEqual(payload["max_tokens"], 4096)
        self.assertEqual(headers, {"Authorization": "Bearer offline-not-a-real-key"})
        self.assertEqual(request.call_args.kwargs, {"timeout": 300})

    def test_container_environment_uses_locked_image_ids(self) -> None:
        lock = {
            "container_runtime": {
                "docker_server_version": "29.1.3",
                "docker_compose_version": "2.40.3",
            },
            "container_images": {
                name: {"image_id": "sha256:" + character * 64}
                for name, character in (("gateway", "1"), ("oa_demo", "2"), ("postgres", "3"))
            },
        }

        def observed(argv: list[str], _label: str, _env: dict[str, str]) -> str:
            if argv[:2] == ["docker", "version"]:
                return "29.1.3"
            if argv[:3] == ["docker", "compose", "version"]:
                return "2.40.3"
            return argv[-1]

        environment: dict[str, str] = {}
        with mock.patch.object(adapter, "command_output", side_effect=observed):
            adapter.configure_locked_container_environment(lock, environment)

        self.assertEqual(environment["WORKFLOW_STUDY_GATEWAY_IMAGE"], "sha256:" + "1" * 64)
        self.assertEqual(environment["WORKFLOW_STUDY_OA_IMAGE"], "sha256:" + "2" * 64)
        self.assertEqual(environment["WORKFLOW_STUDY_POSTGRES_IMAGE"], "sha256:" + "3" * 64)

    def test_container_runtime_drift_aborts_campaign(self) -> None:
        lock = {
            "container_runtime": {
                "docker_server_version": "29.1.3",
                "docker_compose_version": "2.40.3",
            },
            "container_images": {},
        }
        with mock.patch.object(adapter, "command_output", return_value="different"), self.assertRaises(
            adapter.CampaignAbort,
        ):
            adapter.configure_locked_container_environment(lock, {})

    def test_compose_start_has_bounded_fresh_port_retry(self) -> None:
        lock = {
            "infrastructure_retry": {
                "compose_start_max_attempts": 3,
                "compose_start_backoff_seconds": 0.25,
            },
        }
        first_ports = {"GATEWAY": 31001, "OA": 31002}
        second_ports = {"GATEWAY": 32001, "OA": 32002}
        environment: dict[str, str] = {}
        with (
            mock.patch.object(adapter, "free_ports", side_effect=[first_ports, second_ports]),
            mock.patch.object(
                adapter, "run_command", side_effect=[RuntimeError("transient start"), mock.Mock()],
            ) as start,
            mock.patch.object(adapter, "cleanup_compose_project") as cleanup,
            mock.patch.object(adapter.time, "sleep") as sleep,
        ):
            ports, attempts = adapter.start_compose_project("ws-offline", environment, lock)
        self.assertEqual(ports, second_ports)
        self.assertEqual(attempts, 2)
        self.assertEqual(start.call_count, 2)
        cleanup.assert_called_once_with("ws-offline", environment)
        sleep.assert_called_once_with(0.25)
        self.assertEqual(environment["WORKFLOW_STUDY_GATEWAY_PORT"], "32001")
        self.assertEqual(environment["WORKFLOW_STUDY_OA_PORT"], "32002")

    def test_invalid_final_envelope_gets_audited_same_conversation_repair(self) -> None:
        lock = execution_lock()
        lock["max_tool_turns"] = 3
        task = {
            "id": "CAL-OFFLINE",
            "prompt": "Return the registered synthetic result.",
            "products": ["claims"],
            "approved_columns": {"claims": ["claim_id"]},
            "scope": {"claims": {}},
            "required_answer_fields": ["result"],
        }
        provider_call = {
            "request_attempts": 1,
            "successful_responses": 1,
            "retry_attempts": 0,
            "token_usage": {
                "prompt_tokens": 2,
                "prompt_cache_hit_tokens": 0,
                "prompt_cache_miss_tokens": 2,
                "completion_tokens": 1,
                "reasoning_tokens": 0,
                "total_tokens": 3,
            },
            "system_fingerprints": ["fp-v4-test"],
            "finish_reasons": ["stop"],
        }
        state = adapter.empty_run_state()
        responses = [
            ({"role": "assistant", "content": "not json"}, "deepseek-v4-flash", provider_call),
            ({
                "role": "assistant",
                "content": '{"answer":{"result":"ok"},"narrative":"done"}',
            }, "deepseek-v4-flash", provider_call),
        ]
        with mock.patch.object(adapter, "call_deepseek", side_effect=responses):
            answer, narrative = adapter.run_agent(
                {
                    "replicate": 0,
                    "arm": "unlimited",
                    "budget": {},
                    "root_task_id": "offline-root",
                },
                mock.Mock(),
                task,
                lock,
                "offline-not-a-real-key",
                state,
            )
        self.assertEqual(answer, {"result": "ok"})
        self.assertEqual(narrative, "done")
        self.assertEqual(state["final_format_repair_attempts"], 1)
        self.assertEqual(state["provider_api"]["model_turns"], 2)

    def test_parses_usage_fingerprint_and_finish_reason(self) -> None:
        fixture = response(
            finish_reason="tool_calls",
            fingerprint="fp-v4-locked",
            prompt_tokens=17,
            cache_hit_tokens=7,
            completion_tokens=5,
        )
        with mock.patch.object(adapter, "http_json", return_value=fixture):
            message, model, observed = self.call()

        self.assertEqual(message, fixture["choices"][0]["message"])
        self.assertEqual(model, "deepseek-v4-flash")
        self.assertEqual(observed["request_attempts"], 1)
        self.assertEqual(observed["successful_responses"], 1)
        self.assertEqual(observed["retry_attempts"], 0)
        self.assertEqual(observed["system_fingerprints"], ["fp-v4-locked"])
        self.assertEqual(observed["finish_reasons"], ["tool_calls"])
        self.assertEqual(
            observed["token_usage"],
            {
                "prompt_tokens": 17,
                "prompt_cache_hit_tokens": 7,
                "prompt_cache_miss_tokens": 10,
                "completion_tokens": 5,
                "reasoning_tokens": 0,
                "total_tokens": 22,
            },
        )

    def test_429_503_and_transport_failures_retry_with_a_bound(self) -> None:
        failures = [
            adapter.HTTPResponseError(429, "api.deepseek.com", "a" * 64, 99.0),
            adapter.HTTPResponseError(503, "api.deepseek.com", "b" * 64, None),
            adapter.HTTPTransportError("offline transport failure"),
            response(),
        ]
        with (
            mock.patch.object(adapter, "http_json", side_effect=failures) as request,
            mock.patch.object(adapter.time, "sleep") as sleep,
        ):
            _, _, observed = self.call(execution_lock(max_attempts=4))

        self.assertEqual(request.call_count, 4)
        self.assertEqual(observed["request_attempts"], 4)
        self.assertEqual(observed["retry_attempts"], 3)
        self.assertEqual(sleep.call_args_list, [mock.call(0.5), mock.call(0.2), mock.call(0.4)])

    def test_retryable_transport_failure_aborts_at_attempt_bound(self) -> None:
        with (
            mock.patch.object(
                adapter,
                "http_json",
                side_effect=adapter.HTTPTransportError("offline transport failure"),
            ) as request,
            mock.patch.object(adapter.time, "sleep") as sleep,
        ):
            with self.assertRaises(adapter.CampaignAbort):
                self.call(execution_lock(max_attempts=3))

        self.assertEqual(request.call_count, 3)
        self.assertEqual(sleep.call_args_list, [mock.call(0.1), mock.call(0.2)])

    def test_401_and_402_abort_without_retry(self) -> None:
        for status in (401, 402):
            with self.subTest(status=status):
                failure = adapter.HTTPResponseError(
                    status, "api.deepseek.com", str(status) * 32, None
                )
                with (
                    mock.patch.object(adapter, "http_json", side_effect=failure) as request,
                    mock.patch.object(adapter.time, "sleep") as sleep,
                ):
                    with self.assertRaises(adapter.CampaignAbort):
                        self.call()
                self.assertEqual(request.call_count, 1)
                sleep.assert_not_called()

    def test_insufficient_system_resource_response_is_retried_and_audited(self) -> None:
        unavailable = response(
            finish_reason="insufficient_system_resource",
            fingerprint="fp-resource-shortage",
            prompt_tokens=8,
            cache_hit_tokens=3,
            completion_tokens=1,
            content="",
        )
        recovered = response(
            finish_reason="stop",
            fingerprint="fp-recovered",
            prompt_tokens=11,
            cache_hit_tokens=5,
            completion_tokens=4,
            content="recovered",
        )
        with (
            mock.patch.object(adapter, "http_json", side_effect=[unavailable, recovered]) as request,
            mock.patch.object(adapter.time, "sleep") as sleep,
        ):
            message, _, observed = self.call()

        self.assertEqual(message["content"], "recovered")
        self.assertEqual(request.call_count, 2)
        self.assertEqual(observed["request_attempts"], 2)
        self.assertEqual(observed["successful_responses"], 2)
        self.assertEqual(observed["retry_attempts"], 1)
        self.assertEqual(
            observed["finish_reasons"],
            ["insufficient_system_resource", "stop"],
        )
        self.assertEqual(
            observed["system_fingerprints"],
            ["fp-resource-shortage", "fp-recovered"],
        )
        self.assertEqual(
            observed["token_usage"],
            {
                "prompt_tokens": 19,
                "prompt_cache_hit_tokens": 8,
                "prompt_cache_miss_tokens": 11,
                "completion_tokens": 5,
                "reasoning_tokens": 0,
                "total_tokens": 24,
            },
        )
        sleep.assert_called_once_with(0.1)

    def test_inconsistent_token_accounting_aborts_campaign(self) -> None:
        inconsistent = (
            {
                "prompt_tokens": 10,
                "prompt_cache_hit_tokens": 4,
                "prompt_cache_miss_tokens": 5,
                "completion_tokens": 2,
                "total_tokens": 12,
            },
            {
                "prompt_tokens": 10,
                "prompt_cache_hit_tokens": 4,
                "prompt_cache_miss_tokens": 6,
                "completion_tokens": 2,
                "total_tokens": 11,
            },
            {
                "prompt_tokens": 10,
                "prompt_cache_hit_tokens": 4,
                "prompt_cache_miss_tokens": 6,
                "completion_tokens": 2,
                "completion_tokens_details": {"reasoning_tokens": 3},
                "total_tokens": 12,
            },
        )
        for usage in inconsistent:
            with self.subTest(usage=usage):
                with self.assertRaises(adapter.CampaignAbort):
                    adapter.normalize_provider_usage({"usage": usage})


if __name__ == "__main__":
    unittest.main()
