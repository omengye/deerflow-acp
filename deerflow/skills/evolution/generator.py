"""LLM-backed generation of narrowly scoped automatic Skill candidates."""

from __future__ import annotations

import json
import logging
import re
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from deerflow.config import get_app_config
from deerflow.models import aclose_chat_model, create_chat_model
from deerflow.skills.manager import (
    get_custom_skill_dir,
    validate_skill_markdown_content,
    validate_skill_name,
)

from .models import EvolutionSignal

logger = logging.getLogger(__name__)


class GeneratedCandidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    action: Literal["skip", "create", "patch"]
    skill_name: str | None = None
    reason: str = ""
    content: str | None = None
    find: str | None = None
    replace: str | None = None
    expected_count: int | None = Field(default=None, ge=1, le=100)


def extract_json_object(raw: str) -> dict[str, Any] | None:
    """Extract the first balanced JSON object from model output."""
    raw = (raw or "").strip()
    fence = re.match(r"^```(?:json)?\s*\n?(.*?)\n?\s*```$", raw, re.DOTALL | re.IGNORECASE)
    if fence:
        raw = fence.group(1).strip()
    try:
        value = json.loads(raw)
        return value if isinstance(value, dict) else None
    except json.JSONDecodeError:
        pass
    start = raw.find("{")
    if start < 0:
        return None
    depth = 0
    in_string = False
    escaped = False
    for index in range(start, len(raw)):
        char = raw[index]
        if escaped:
            escaped = False
            continue
        if char == "\\":
            escaped = True
            continue
        if char == '"':
            in_string = not in_string
            continue
        if in_string:
            continue
        if char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                try:
                    value = json.loads(raw[start : index + 1])
                    return value if isinstance(value, dict) else None
                except json.JSONDecodeError:
                    return None
    return None


def _normalize_generated_skill_name(name: str) -> str:
    """Convert common model naming variants to the required safe slug."""
    normalized = re.sub(r"[^a-z0-9]+", "-", name.strip().lower()).strip("-")
    if not normalized:
        raise ValueError("Generated skill_name cannot be normalized to hyphen-case.")
    return validate_skill_name(normalized)


def _set_generated_frontmatter_name(content: str, name: str) -> str:
    """Keep a generated create candidate's frontmatter aligned with its slug."""
    match = re.match(r"^(---\n)(.*?)(\n---(?:\n|$))", content, re.DOTALL)
    if match is None:
        return content
    frontmatter, replacements = re.subn(r"(?m)^name\s*:.*$", f"name: {name}", match.group(2), count=1)
    if replacements == 0:
        return content
    return f"{match.group(1)}{frontmatter}{match.group(3)}{content[match.end():]}"


class SkillCandidateGenerator:
    """Generate only skip/create/exact patch decisions from sanitized signals."""

    def _skill_context(self, signal: EvolutionSignal) -> str:
        blocks: list[str] = []
        seen: set[str] = set()
        for used in signal.skills_used:
            if used.get("scope") != "custom":
                continue
            name = str(used.get("name") or "")
            if not name or name in seen:
                continue
            seen.add(name)
            path = get_custom_skill_dir(name) / "SKILL.md"
            if not path.is_file():
                continue
            try:
                content = path.read_text(encoding="utf-8")
            except OSError:
                continue
            blocks.append(f"CUSTOM SKILL {name}:\n-----\n{content[:20_000]}\n-----")
        return "\n\n".join(blocks) or "No custom Skill used in this task."

    @staticmethod
    def _validate(candidate: GeneratedCandidate) -> GeneratedCandidate:
        if candidate.action == "skip":
            return candidate
        if not candidate.skill_name:
            raise ValueError("Generated candidate is missing skill_name.")
        candidate.skill_name = _normalize_generated_skill_name(candidate.skill_name)
        if candidate.action == "create":
            if not candidate.content:
                raise ValueError("Generated create candidate is missing content.")
            candidate.content = _set_generated_frontmatter_name(candidate.content, candidate.skill_name)
            validate_skill_markdown_content(candidate.skill_name, candidate.content)
            if get_custom_skill_dir(candidate.skill_name).exists():
                raise ValueError("Generated create candidate targets an existing custom Skill.")
        elif candidate.action == "patch":
            skill_file = get_custom_skill_dir(candidate.skill_name) / "SKILL.md"
            if not skill_file.is_file():
                raise ValueError("Generated patch candidate must target an existing custom Skill.")
            if not candidate.find or candidate.replace is None or candidate.find == candidate.replace:
                raise ValueError("Generated patch candidate needs distinct, non-empty find/replace text.")
            current = skill_file.read_text(encoding="utf-8")
            occurrences = current.count(candidate.find)
            expected = candidate.expected_count or 1
            if occurrences != expected:
                raise ValueError(f"Generated patch expected {expected} exact matches but found {occurrences}.")
            candidate.expected_count = expected
        return candidate

    async def generate(self, signal: EvolutionSignal) -> GeneratedCandidate:
        rubric = (
            "You improve reusable AI-agent Skills from a sanitized task signal. "
            "Return ONLY one JSON object. Allowed actions are skip, create, patch. "
            "Prefer an exact patch to a custom Skill that was actually used. "
            "skill_name must use lowercase ASCII letters, digits, and single hyphens only, with at most 64 characters. "
            "For patch, copy an existing custom Skill name exactly. For create, use the same skill_name in the SKILL.md frontmatter. "
            "For patch, provide verbatim find and replace strings and expected_count. "
            "Never propose scripts, support files, shell commands, credentials, permissions, URLs, deletion, or direct file writes. "
            "Use create only for a clearly reusable workflow when no existing custom Skill fits. "
            "Use skip when evidence is weak or the task is one-off. Schema: "
            '{"action":"skip|create|patch","skill_name":null,"reason":"...",'
            '"content":null,"find":null,"replace":null,"expected_count":null}'
        )
        payload = {
            "triggers": signal.trigger_types,
            "user_summary": signal.user_summary,
            "assistant_summary": signal.assistant_summary,
            "tool_names": signal.tool_names,
            "tool_count": signal.tool_count,
            "tool_error_count": signal.tool_error_count,
            "recovered_error_count": signal.recovered_error_count,
            "unresolved_error_count": signal.unresolved_error_count,
            "recurrence_count": signal.recurrence_count,
            "skills_used": signal.skills_used,
        }
        prompt = f"Signal:\n{json.dumps(payload, ensure_ascii=False)}\n\nAvailable context:\n{self._skill_context(signal)}"
        model = None
        try:
            config = get_app_config().skill_evolution
            model = create_chat_model(
                name=config.generation_model_name,
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
                    "thread_id": signal.thread_id or f"skill-evolution-{signal.id}",
                    "run_id": signal.run_id or signal.id,
                },
            )
            async with llm_call_slot_async():
                response = await request_model.ainvoke(
                    [{"role": "system", "content": rubric}, {"role": "user", "content": prompt}],
                    config={"run_name": "skill_evolution_generator"},
                )
            parsed = extract_json_object(str(getattr(response, "content", "") or ""))
            if parsed is None:
                raise ValueError("Candidate generator returned malformed JSON.")
            return self._validate(GeneratedCandidate.model_validate(parsed))
        except (ValidationError, ValueError) as exc:
            logger.warning("Automatic Skill candidate was rejected: %s", exc)
            return GeneratedCandidate(action="skip", reason="Generator output was invalid or unsafe.")
        except Exception:
            logger.warning("Automatic Skill candidate generation failed", exc_info=True)
            return GeneratedCandidate(action="skip", reason="Generator unavailable; manual behavior retained.")
        finally:
            await aclose_chat_model(model)
