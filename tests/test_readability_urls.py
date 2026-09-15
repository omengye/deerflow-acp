from __future__ import annotations

from deerflow.utils.readability import _resolve_html_urls


def test_resolve_html_urls_uses_fetched_url_and_base_element() -> None:
    html = """
    <html><head><base href="../assets/"></head><body>
      <a href="docs/page.html">Docs</a>
      <img src="images/pic.png">
      <a href="https://other.example/x">Absolute</a>
    </body></html>
    """

    resolved = _resolve_html_urls(html, "https://example.com/guide/start/")

    assert 'href="https://example.com/guide/assets/docs/page.html"' in resolved
    assert 'src="https://example.com/guide/assets/images/pic.png"' in resolved
    assert 'href="https://other.example/x"' in resolved


def test_resolve_html_urls_does_not_rewrite_markup_inside_script_text() -> None:
    html = '<script>const sample = \'<a href="relative">x</a>\';</script><a href="real">real</a>'

    resolved = _resolve_html_urls(html, "https://example.com/root/")

    assert '<a href="relative">x</a>' in resolved
    assert 'href="https://example.com/root/real"' in resolved
