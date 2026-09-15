import base64
from contextlib import ExitStack
import os

import requests

OPENAI_DEFAULT_BASE_URL = "https://api.openai.com/v1"
OPENAI_DEFAULT_MODEL = "gpt-image-2.5-flare"


def validate_image(image_path: str) -> bool:
    """
    Validate if an image file can be opened and is not corrupted.
    
    Args:
        image_path: Path to the image file
        
    Returns:
        True if the image is valid and can be opened, False otherwise
    """
    from PIL import Image

    try:
        with Image.open(image_path) as img:
            img.verify()  # Verify that it's a valid image
        # Re-open to check if it can be fully loaded (verify() may not catch all issues)
        with Image.open(image_path) as img:
            img.load()  # Force load the image data
        return True
    except Exception as e:
        print(f"Warning: Image '{image_path}' is invalid or corrupted: {e}")
        return False


def _resolve_provider() -> str:
    override = os.getenv("IMAGE_GENERATION_PROVIDER")
    if override:
        return override.strip().lower()
    if os.getenv("GEMINI_API_KEY"):
        return "gemini"
    if os.getenv("IMAGE_GENERATION_API_KEY"):
        return "openai"
    raise ValueError(
        "No image credentials found. Set GEMINI_API_KEY or "
        "IMAGE_GENERATION_API_KEY (and optionally IMAGE_GENERATION_PROVIDER)."
    )


def _openai_base_url() -> str:
    return os.getenv("IMAGE_GENERATION_BASE_URL", OPENAI_DEFAULT_BASE_URL).rstrip("/")


def _openai_size(aspect_ratio: str, model: str) -> str:
    override = os.getenv("IMAGE_GENERATION_SIZE")
    if override:
        allowed = {
            "dall-e-2": {"256x256", "512x512", "1024x1024"},
            "dall-e-3": {"1024x1024", "1792x1024", "1024x1792"},
        }.get(model)
        if allowed is not None and override not in allowed:
            raise ValueError(
                f"{model} size must be one of: {', '.join(sorted(allowed))}"
            )
        return override
    portrait = aspect_ratio in {"9:16", "2:3", "3:4"}
    landscape = aspect_ratio in {"16:9", "3:2", "4:3"}
    if model == "dall-e-2":
        return "1024x1024"
    if model == "dall-e-3":
        return "1024x1792" if portrait else "1792x1024" if landscape else "1024x1024"
    return "1024x1536" if portrait else "1536x1024" if landscape else "1024x1024"


def _guess_mime(image_path: str) -> str:
    return {
        ".png": "image/png",
        ".webp": "image/webp",
        ".gif": "image/gif",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
    }.get(os.path.splitext(image_path)[1].lower(), "image/jpeg")


def _output_format(output_file: str) -> str:
    extension = os.path.splitext(output_file)[1].lower()
    if extension in {".jpg", ".jpeg"}:
        return "jpeg"
    if extension == ".webp":
        return "webp"
    return "png"


def _ensure_output_dir(output_file: str) -> None:
    output_dir = os.path.dirname(output_file)
    if output_dir:
        os.makedirs(output_dir, exist_ok=True)


def _write_openai_image(payload: dict, output_file: str) -> str:
    images = payload.get("data") or []
    if not images or not isinstance(images[0], dict):
        raise Exception("OpenAI-compatible provider returned no image data")
    item = images[0]
    encoded = item.get("b64_json") or item.get("image_base64") or item.get("base64")
    if encoded:
        image_bytes = base64.b64decode(encoded)
    else:
        image_url = item.get("url")
        if not image_url:
            raise Exception(
                "OpenAI-compatible provider returned neither base64 data nor a URL"
            )
        if image_url.startswith("data:"):
            _, encoded = image_url.split(",", 1)
            image_bytes = base64.b64decode(encoded)
        else:
            download = requests.get(image_url, timeout=120)
            download.raise_for_status()
            image_bytes = download.content
    _ensure_output_dir(output_file)
    with open(output_file, "wb") as stream:
        stream.write(image_bytes)
    return f"Successfully generated image to {output_file}"


def _generate_image_openai(
    prompt: str,
    reference_images: list[str],
    output_file: str,
    aspect_ratio: str,
) -> str:
    api_key = os.getenv("IMAGE_GENERATION_API_KEY")
    if not api_key:
        return "IMAGE_GENERATION_API_KEY is not set"
    model = os.getenv("IMAGE_GENERATION_MODEL", OPENAI_DEFAULT_MODEL)
    is_dall_e = model in {"dall-e-2", "dall-e-3"}
    if is_dall_e and os.path.splitext(output_file)[1].lower() != ".png":
        raise ValueError("DALL-E output files must use a .png extension")
    if is_dall_e and reference_images:
        raise ValueError(f"{model} reference-image editing is not supported")

    fields = {
        "model": model,
        "prompt": prompt,
        "n": 1,
        "size": _openai_size(aspect_ratio, model),
    }
    if is_dall_e:
        fields["response_format"] = "b64_json"
    else:
        fields["output_format"] = _output_format(output_file)

    headers = {"Authorization": f"Bearer {api_key}"}
    if reference_images:
        with ExitStack() as stack:
            files = [
                (
                    "image[]",
                    (
                        os.path.basename(path),
                        stack.enter_context(open(path, "rb")),
                        _guess_mime(path),
                    ),
                )
                for path in reference_images
            ]
            response = requests.post(
                f"{_openai_base_url()}/images/edits",
                headers=headers,
                data=fields,
                files=files,
                timeout=180,
            )
    else:
        response = requests.post(
            f"{_openai_base_url()}/images/generations",
            headers={**headers, "Content-Type": "application/json"},
            json=fields,
            timeout=180,
        )
    response.raise_for_status()
    return _write_openai_image(response.json(), output_file)


def generate_image(
    prompt_file: str,
    reference_images: list[str],
    output_file: str,
    aspect_ratio: str = "16:9",
) -> str:
    with open(prompt_file, "r", encoding="utf-8") as f:
        prompt = f.read()
    provider = _resolve_provider()
    if provider in {"openai", "openai-compatible"}:
        return _generate_image_openai(
            prompt,
            reference_images,
            output_file,
            aspect_ratio,
        )
    if provider not in {"gemini", "google"}:
        raise ValueError(
            f"Unknown image provider: {provider!r} "
            "(use 'gemini', 'openai', or 'openai-compatible')"
        )
    parts = []
    i = 0
    
    # Filter out invalid reference images
    valid_reference_images = []
    for ref_img in reference_images:
        if validate_image(ref_img):
            valid_reference_images.append(ref_img)
        else:
            print(f"Skipping invalid reference image: {ref_img}")
    
    if len(valid_reference_images) < len(reference_images):
        print(f"Note: {len(reference_images) - len(valid_reference_images)} reference image(s) were skipped due to validation failure.")
    
    for reference_image in valid_reference_images:
        i += 1
        with open(reference_image, "rb") as f:
            image_b64 = base64.b64encode(f.read()).decode("utf-8")
        parts.append(
            {
                "inlineData": {
                    "mimeType": "image/jpeg",
                    "data": image_b64,
                }
            }
        )

    api_key = os.getenv("GEMINI_API_KEY")
    if not api_key:
        return "GEMINI_API_KEY is not set"
    response = requests.post(
        "https://generativelanguage.googleapis.com/v1beta/models/gemini-3-pro-image-preview:generateContent",
        headers={
            "x-goog-api-key": api_key,
            "Content-Type": "application/json",
        },
        json={
            "generationConfig": {"imageConfig": {"aspectRatio": aspect_ratio}},
            "contents": [{"parts": [*parts, {"text": prompt}]}],
        },
        timeout=180,
    )
    response.raise_for_status()
    json = response.json()
    parts: list[dict] = json["candidates"][0]["content"]["parts"]
    image_parts = [part for part in parts if part.get("inlineData", False)]
    if len(image_parts) == 1:
        base64_image = image_parts[0]["inlineData"]["data"]
        # Save the image to a file
        _ensure_output_dir(output_file)
        with open(output_file, "wb") as f:
            f.write(base64.b64decode(base64_image))
        return f"Successfully generated image to {output_file}"
    else:
        raise Exception("Failed to generate image")


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(
        description="Generate images using Gemini or an OpenAI-compatible API"
    )
    parser.add_argument(
        "--prompt-file",
        required=True,
        help="Absolute path to JSON prompt file",
    )
    parser.add_argument(
        "--reference-images",
        nargs="*",
        default=[],
        help="Absolute paths to reference images (space-separated)",
    )
    parser.add_argument(
        "--output-file",
        required=True,
        help="Output path for generated image",
    )
    parser.add_argument(
        "--aspect-ratio",
        required=False,
        default="16:9",
        help="Aspect ratio of the generated image",
    )

    args = parser.parse_args()

    try:
        print(
            generate_image(
                args.prompt_file,
                args.reference_images,
                args.output_file,
                args.aspect_ratio,
            )
        )
    except Exception as e:
        print(f"Error while generating image: {e}")
