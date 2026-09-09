"""Independent safety and quality gate for automatic Skill patches."""

from __future__ import annotations

import logging
import re
from typing import Any

from deerflow.config import get_app_config
from deerflow.models import aclose_chat_model, create_chat_model
from deerflow.skills.manager import get_custom_skill_dir

from .generator import extract_json_object
from .models import EvolutionSignal, SkillProposal
from .store import FileEvolutionStore, hash_skill_tree

logger = logging.getLogger(__name__)


_FORBIDDEN_ADDITION_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("url", re.compile(r"https?://", re.IGNORECASE)),
    ("shell", re.compile(r"\b(?:bash|sh|zsh|cmd|pwsh|powershell|curl|wget|subprocess|os\.system)\b", re.IGNORECASE)),
    ("permission", re.compile(r"\b(?:sudo|chmod|chown|administrator|privilege|permissions?)\b", re.IGNORECASE)),
    ("environment", re.compile(r"(?:\$\{?[A-Z_][A-Z0-9_]*\}?|os\.environ|getenv\(|process\.env)", re.IGNORECASE)),
    ("credential", re.compile(r"\b(?:api[-_ ]?key|secret|token|credential|password)\b", re.IGNORECASE)),
)


def _diff_metrics(diff: str) -> tuple[int, list[str]]:
    changed = 0
    added: list[str] = []
    for line in diff.splitlines():
        if line.startswith(("+++", "---", "@@")):
            continue
        if line.startswith("+"):
            changed += 1
            added.append(line[1:])
        elif line.startswith("-"):
            changed += 1
    return changed, added


class AutoPatchEvaluator:
    """Fail-to-review gate; it never grants publication on malformed evidence."""

    def __init__(self, store: FileEvolutionStore | None = None):
        self.store = store or FileEvolutionStore()

    def deterministic_checks(self, proposal: SkillProposal) -> dict[str, Any]:
        reasons: list[str] = []
        config = get_app_config().skill_evolution
        active = get_custom_skill_dir(proposal.skill_name)
        diff = self.store.read_proposal_diff(proposal.id)
        changed_lines, additions = _diff_metrics(diff)

        if config.mode != "auto_patch":
            reasons.append("auto_patch mode is disabled")
        if proposal.origin != "automatic":
            reasons.append("proposal is not automatic")
        if proposal.action != "patch":
            reasons.append("only exact patches can auto-publish")
        if not active.is_dir():
            reasons.append("target is not an existing custom Skill")
        if proposal.changed_files != ["SKILL.md"]:
            reasons.append("patch changes files outside SKILL.md")
        if proposal.risk != "low":
            reasons.append("proposal risk is not low")
        if not proposal.scans or any(scan.get("decision") != "allow" for scan in proposal.scans):
            reasons.append("all security scans must explicitly allow")
        if changed_lines > config.auto_patch.max_changed_lines:
            reasons.append(f"patch changes {changed_lines} lines; limit is {config.auto_patch.max_changed_lines}")
        if proposal.base_sha256 != hash_skill_tree(active):
            reasons.append("proposal base digest is stale")
        added_text = "\n".join(additions)
        for label, pattern in _FORBIDDEN_ADDITION_PATTERNS:
            if pattern.search(added_text):
                reasons.append(f"patch adds restricted {label} content")

        return {
            "decision": "allow" if not reasons else "review",
            "source": "deterministic",
            "reason": "; ".join(reasons) if reasons else "Deterministic auto-patch checks passed.",
            "changed_lines": changed_lines,
            "checks": {"restricted_additions": not any("restricted" in reason for reason in reasons)},
        }

    async def evaluate(self, proposal: SkillProposal, signal: EvolutionSignal | None = None) -> dict[str, Any]:
        deterministic = self.deterministic_checks(proposal)
        if deterministic["decision"] != "allow":
            return deterministic

        rubric = (
            "You are an independent quality gate for a small AI-agent Skill patch. "
            "Allow only when the patch directly addresses the observed reusable problem, preserves existing behavior, "
            "is precise, and introduces no new authority or operational capability. "
            "Return ONLY JSON: {\"decision\":\"allow|review\",\"reason\":\"...\"}."
        )
        prompt = (
            f"Signal: {(signal.model_dump_json() if signal else 'unavailable')}\n\n"
            f"Proposal: {proposal.model_dump_json()}\n\nDiff:\n{self.store.read_proposal_diff(proposal.id)}"
        )
        model = None
        try:
            config = get_app_config().skill_evolution
            model = create_chat_model(
                name=config.evaluation_model_name,
                thinking_enabled=False,
                disable_keepalive=True,
            )
            from deerflow.agents.middlewares.llm_error_handling_middleware import (
                llm_call_slot_async,
            )
            from deerflow.agents.middlewares.runtime_headers_middleware import (
                bind_runtime_headers,
            )

            request_model = bind_runtime_headers(
                model,
                runtime_values={
                    "thread_id": (
                        (signal.thread_id if signal is not None else None)
                        or proposal.trigger.thread_id
                        or f"skill-evolution-{proposal.id}"
                    ),
                    "run_id": (
                        (signal.run_id if signal is not None else None)
                        or proposal.id
                    ),
                },
            )
            async with llm_call_slot_async():
                response = await request_model.ainvoke(
                    [{"role": "system", "content": rubric}, {"role": "user", "content": prompt}],
                    config={"run_name": "skill_evolution_evaluator"},
                )
            parsed = extract_json_object(str(getattr(response, "content", "") or "")) or {}
            decision = str(parsed.get("decision") or "").lower()
            if decision not in {"allow", "review"}:
                raise ValueError("Evaluator returned an invalid decision.")
            return {
                **deterministic,
                "decision": decision,
                "source": "deterministic+llm",
                "reason": str(parsed.get("reason") or "No evaluation reason provided."),
            }
        except Exception:
            logger.warning("Automatic Skill patch evaluation failed; routing to review", exc_info=True)
            return {
                **deterministic,
                "decision": "review",
                "source": "deterministic+llm",
                "reason": "Independent evaluation was unavailable or malformed; manual review required.",
            }
        finally:
            await aclose_chat_model(model)
