"""Model management endpoints."""
import logging
from typing import Any

from fastapi import APIRouter, HTTPException

from app.dependencies import get_client_manager
from app.schemas import ModelInfo, ModelListResponse

logger = logging.getLogger(__name__)

router = APIRouter(tags=["models"])


@router.get("/models", response_model=ModelListResponse)
async def list_models():
    """List all available LLM models from configuration."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        result = client.list_models()
        models = []
        if isinstance(result, dict) and "models" in result:
            for m in result["models"]:
                models.append(ModelInfo(
                    name=m.get("name", ""),
                    display_name=m.get("display_name") or m.get("name", ""),
                    supports_thinking=m.get("supports_thinking", False),
                    supports_vision=m.get("supports_vision", False),
                ))
        return ModelListResponse(models=models)
    except Exception as e:
        logger.exception("Unhandled error")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/models/{model_name}")
async def get_model(model_name: str):
    """Get details for a specific model."""
    manager = get_client_manager()
    client = manager.get_client()

    try:
        model = client.get_model(model_name)
        if not model:
            raise HTTPException(status_code=404, detail=f"Model '{model_name}' not found")
        return model
    except HTTPException:
        raise
    except Exception as e:
        logger.exception("Unhandled error")
        raise HTTPException(status_code=500, detail="Internal server error")
