---
name: systematic-literature-review
description: Use when surveying multiple academic papers on a research topic, comparing their methods and findings, and producing a cited synthesis. A single-paper critique belongs to academic-paper-review.
---

# Systematic Literature Review

Define the research question, date range, inclusion criteria, target corpus and citation style from the user's request. Use reasonable stated defaults when omitted; avoid silently claiming exhaustive coverage. For large surveys, split the topic into manageable groups and report coverage.

## Search and screen

Use the available Go-native `web_search` and `web_fetch` tools. Start with short keyword queries such as `site:arxiv.org/abs diffusion models cs.CV`. Fetch each selected paper's actual abstract or primary publication page. When the configured fetch tool supports Atom/XML, the public arXiv API at `https://export.arxiv.org/api/query` is another source; use its documented query, category, date and pagination parameters. A connector can supply additional scholarly indexes when explicitly available.

Keep a search record with queries, sources, date, candidates and reasons for inclusion/exclusion. Respect rate limits; an unavailable page is missing evidence, not a failed paper. Deduplicate by DOI or normalized arXiv ID, retaining the specific version read. Verify title, authors, publication date and source URL. Distinguish preprints from peer-reviewed publications.

## Extract evidence

For each included paper, record its research question, methods, findings, limitations, and passages or sections supporting those claims. If only the abstract is available, label the extraction accordingly and avoid inventing experiments or metrics.

For independent batches, use available subagent tools with the actual paper text or verified excerpts. Use their current schemas and host-reported concurrency limits. Inspect returned task state before consuming a result; a failed, cancelled or incomplete task is not evidence. If delegation is unavailable, process bounded batches serially and state the achieved coverage.

## Synthesize

Compare approaches and results across papers: recurring themes, agreements, disagreements, methodological differences and open questions. Avoid treating unlike datasets or metrics as directly comparable. A small or heterogeneous corpus may not support strong thematic conclusions.

Read the template matching the requested citation style:

- [APA](templates/apa.md): default author/date report.
- [IEEE](templates/ieee.md): numbered citations.
- [BibTeX](templates/bibtex.md): machine-readable bibliography; use `@misc` for unreviewed arXiv preprints.

Keep inline citations attached to the claims they support. Include selection methodology and missing-source limitations. Save the report to the requested output location, or the active workspace's output directory, and present its actual path with a short summary. Use a file presentation tool only when the runtime exposes one.
