"""Token-authenticated Skill Proposal management for the local ACP daemon."""

from __future__ import annotations

import logging
import re
from typing import Any

from deerflow.skills.evolution import (
    SkillEvolutionService,
    SkillPublishConflict,
    get_evolution_store,
)


_PROPOSAL_ID_RE = re.compile(r"^p_[A-Za-z0-9_-]{1,128}$")
_PROPOSAL_STATUSES = {
    "generating",
    "validating",
    "pending_review",
    "publishing",
    "published",
    "rejected",
    "failed",
    "stale",
}
_MAX_NOTE_LENGTH = 4000
logger = logging.getLogger(__name__)


def _success(data: dict[str, Any]) -> dict[str, Any]:
    return {"ok": True, "data": data}


def _failure(error: str, code: str) -> dict[str, Any]:
    return {"ok": False, "error": error, "code": code}


def _proposal_id(request: dict[str, Any]) -> str:
    proposal_id = request.get("proposal_id")
    if not isinstance(proposal_id, str) or not _PROPOSAL_ID_RE.fullmatch(
        proposal_id
    ):
        raise ValueError("Invalid Skill proposal id.")
    return proposal_id


def _optional_note(request: dict[str, Any]) -> str | None:
    note = request.get("note")
    if note is None:
        return None
    if not isinstance(note, str):
        raise ValueError("note must be a string or null.")
    if len(note) > _MAX_NOTE_LENGTH:
        raise ValueError(f"note must be at most {_MAX_NOTE_LENGTH} characters.")
    return note.strip() or None


def _optional_base_digest(request: dict[str, Any]) -> str | None:
    digest = request.get("expected_base_sha256")
    if digest is None:
        return None
    if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", digest):
        raise ValueError("expected_base_sha256 must be a SHA-256 digest or null.")
    return digest.lower()


async def _dispatch(request: dict[str, Any]) -> dict[str, Any]:
    operation = request.get("operation")
    if not isinstance(operation, str):
        raise ValueError("operation must be a string.")

    store = get_evolution_store()
    if operation == "proposal.history":
        revisions = []
        for directory in sorted(store.revisions_dir.iterdir()) if store.revisions_dir.exists() else []:
            if directory.is_dir():
                revisions.extend({**r.model_dump(mode="json"), "name": r.skill_name} for r in store.list_revisions(directory.name))
        return {"proposals": [p.model_dump(mode="json") for p in store.list_proposals(include_archived=True)],
                "revisions": revisions, "signals": [s.model_dump(mode="json") for s in store.list_signals()],
                "probations": store.get_probations(),
                "catalog_version": store.get_catalog_version()}
    if operation == "proposal.rollback":
        from deerflow.skills.evolution.publisher import SkillPublisher
        name = request.get("name")
        version = request.get("version")
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", name):
            raise ValueError("Invalid Skill name")
        if type(version) is not int or version < 1:
            raise ValueError("Invalid Skill revision")
        return SkillPublisher(store).rollback(name, version, actor="desktop", note=_optional_note(request))
    if operation == "proposal.list":
        status = request.get("status")
        if status is not None and (
            not isinstance(status, str) or status not in _PROPOSAL_STATUSES
        ):
            raise ValueError("Invalid Skill proposal status.")
        proposals = store.list_proposals(
            status=status,
            include_archived=False,
        )
        return {
            "proposals": [proposal.model_dump(mode="json") for proposal in proposals],
            "catalog_version": store.get_catalog_version(),
        }

    if operation not in {"proposal.get", "proposal.approve", "proposal.reject"}:
        raise ValueError(f"Unsupported management operation: {operation}")

    proposal_id = _proposal_id(request)
    if operation == "proposal.get":
        proposal = store.load_proposal(proposal_id)
        return {
            **proposal.model_dump(mode="json"),
            "diff": store.read_proposal_diff(proposal_id),
        }

    service = SkillEvolutionService(store)
    if operation == "proposal.approve":
        proposal = await service.approve_proposal(
            proposal_id,
            expected_base_sha256=_optional_base_digest(request),
            note=_optional_note(request),
            actor="desktop",
        )
        from deerflow.agents.lead_agent.prompt import (
            refresh_skills_system_prompt_cache_async,
        )

        try:
            await refresh_skills_system_prompt_cache_async()
        except Exception:
            logger.debug(
                "Failed to refresh skills prompt cache after desktop approval",
                exc_info=True,
            )
        return {
            "proposal": proposal.model_dump(mode="json"),
            "catalog_version": store.get_catalog_version(),
        }

    if operation == "proposal.reject":
        proposal = service.reject_proposal(
            proposal_id,
            note=_optional_note(request),
            actor="desktop",
        )
        return {
            "proposal": proposal.model_dump(mode="json"),
            "catalog_version": store.get_catalog_version(),
        }

    raise AssertionError("unreachable Proposal management operation")


async def handle_proposal_management_request(
    request: dict[str, Any],
) -> dict[str, Any]:
    """Execute one bounded Proposal operation and return a JSON envelope."""

    try:
        return _success(await _dispatch(request))
    except SkillPublishConflict as exc:
        return _failure(str(exc), "conflict")
    except FileNotFoundError as exc:
        return _failure(str(exc), "not_found")
    except ValueError as exc:
        return _failure(str(exc), "invalid_request")
    except Exception as exc:
        return _failure(str(exc), "internal_error")
