"""Small asynchronous client for the RAGFlow retrieval APIs."""

from __future__ import annotations

from typing import Any

import httpx

_DATASET_PAGE_SIZE = 100
_MAX_DATASET_PAGES = 100


class RAGFlowError(Exception):
    """Base class for normalized provider failures."""


class RAGFlowAPIError(RAGFlowError):
    """RAGFlow returned a non-zero API result code."""

    def __init__(self, message: str, *, code: object = None) -> None:
        self.code = code
        super().__init__(message)


class RAGFlowConnectionError(RAGFlowError):
    """RAGFlow could not be reached before the configured timeout."""


class RAGFlowProtocolError(RAGFlowError):
    """RAGFlow returned an invalid HTTP or JSON response."""


class RAGFlowClient:
    """Stateless client for dataset discovery and read-only retrieval."""

    def __init__(
        self,
        *,
        base_url: str,
        api_key: str,
        timeout: float = 30,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self._api_key = api_key
        self._transport = transport

    def _redact(self, value: object) -> str:
        text = str(value)
        return text.replace(self._api_key, "[REDACTED]") if self._api_key else text

    async def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, object] | None = None,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        client_options: dict[str, Any] = {
            "base_url": f"{self.base_url}/api/v1",
            "headers": {
                "Authorization": f"Bearer {self._api_key}",
                "Accept": "application/json",
            },
            "timeout": self.timeout,
        }
        if self._transport is not None:
            client_options["transport"] = self._transport

        try:
            async with httpx.AsyncClient(**client_options) as client:
                response = await client.request(
                    method,
                    path,
                    params=params,
                    json=json,
                )
        except httpx.TimeoutException:
            raise RAGFlowConnectionError(
                f"RAGFlow request timed out after {self.timeout:g} seconds."
            ) from None
        except httpx.RequestError as exc:
            raise RAGFlowConnectionError(
                f"{type(exc).__name__}: {self._redact(exc)}"
            ) from None

        try:
            payload = response.json()
        except ValueError:
            raise RAGFlowProtocolError("RAGFlow returned invalid JSON.") from None
        if not isinstance(payload, dict):
            raise RAGFlowProtocolError("RAGFlow returned a non-object JSON payload.")

        if response.is_error:
            code = payload.get("code")
            if code not in (None, 0):
                detail = payload.get("message") or (
                    f"RAGFlow API error (HTTP {response.status_code})"
                )
                raise RAGFlowAPIError(self._redact(detail), code=code)
            raise RAGFlowProtocolError(
                f"RAGFlow request failed (HTTP {response.status_code})."
            )

        code = payload.get("code")
        if code != 0:
            detail = payload.get("message") or "RAGFlow request failed."
            raise RAGFlowAPIError(self._redact(detail), code=code)
        return payload

    async def list_datasets(
        self,
        *,
        dataset_id: str | None = None,
    ) -> list[dict[str, Any]]:
        """Resolve one dataset ID or enumerate every accessible dataset."""
        if dataset_id is not None:
            dataset_id = dataset_id.strip()
            if not dataset_id:
                raise ValueError("dataset_id must not be empty")
            payload = await self._request(
                "GET",
                "/datasets",
                params={"ids": dataset_id},
            )
            data = payload.get("data")
            if not isinstance(data, list):
                raise RAGFlowProtocolError("RAGFlow returned an invalid dataset list.")
            return [item for item in data if isinstance(item, dict)]

        datasets: list[dict[str, Any]] = []
        received = 0
        for page in range(1, _MAX_DATASET_PAGES + 1):
            payload = await self._request(
                "GET",
                "/datasets",
                params={"page": page, "page_size": _DATASET_PAGE_SIZE},
            )
            data = payload.get("data")
            if not isinstance(data, list):
                raise RAGFlowProtocolError("RAGFlow returned an invalid dataset list.")
            datasets.extend(item for item in data if isinstance(item, dict))
            received += len(data)

            total = payload.get("total")
            if not isinstance(total, int) or isinstance(total, bool) or total < 0:
                total = payload.get("total_datasets")
            if isinstance(total, int) and not isinstance(total, bool) and total >= 0:
                if received >= total:
                    return datasets
                if not data:
                    raise RAGFlowProtocolError(
                        "RAGFlow dataset listing ended before its reported total."
                    )
            elif len(data) < _DATASET_PAGE_SIZE:
                return datasets

        raise RAGFlowProtocolError(
            f"RAGFlow dataset listing exceeded {_MAX_DATASET_PAGES} pages."
        )

    async def retrieve(
        self,
        query: str,
        *,
        dataset_ids: list[str],
        page_size: int,
        similarity_threshold: float,
        vector_similarity_weight: float,
        top_k: int,
        document_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Retrieve chunks from an explicit operator-resolved dataset scope."""
        if not dataset_ids or not all(
            isinstance(item, str) and item.strip() for item in dataset_ids
        ):
            raise ValueError("dataset_ids must contain at least one dataset ID")
        payload = await self._request(
            "POST",
            "/retrieval",
            json={
                "question": query,
                "dataset_ids": dataset_ids,
                "page_size": page_size,
                "similarity_threshold": similarity_threshold,
                "vector_similarity_weight": vector_similarity_weight,
                "top_k": top_k,
                **({"document_ids": document_ids} if document_ids is not None else {}),
            },
        )
        data = payload.get("data")
        if not isinstance(data, dict):
            raise RAGFlowProtocolError("RAGFlow returned an invalid retrieval result.")
        return data

    async def validate_documents(self, dataset_id: str, document_ids: list[str]) -> None:
        """Validate large selections using bounded pages, not oversized URLs."""
        from urllib.parse import quote

        remaining = set(document_ids)
        if not remaining:
            return
        for page in range(1, _MAX_DATASET_PAGES + 1):
            payload = await self._request("GET", f"/datasets/{quote(dataset_id, safe='')}/documents", params={"page": page, "page_size": _DATASET_PAGE_SIZE})
            data = payload.get("data")
            if not isinstance(data, dict) or not isinstance(data.get("docs"), list):
                raise RAGFlowProtocolError("RAGFlow returned an invalid document list.")
            docs = data["docs"]
            remaining.difference_update(item.get("id") for item in docs if isinstance(item, dict))
            if not remaining:
                return
            total = data.get("total")
            if not docs or (isinstance(total, int) and page * _DATASET_PAGE_SIZE >= total):
                break
        raise RAGFlowProtocolError("Selected knowledge documents are unavailable or inaccessible.")
