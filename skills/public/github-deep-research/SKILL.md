---
name: github-deep-research
description: Use when investigating a GitHub repository's architecture, development history, maintenance, issues or alternatives using primary sources.
---

# GitHub Deep Research

Start with the exact repository URL and the user's question. Use the available Go-native `web_search` and `web_fetch` tools. If an authenticated GitHub or OpenCLI tool is explicitly available, use its documented schema for private or rate-limited content. Do not assume a local script, browser login or credential exists.

## Establish the repository

Read the repository page, README and relevant source tree before drawing conclusions. For public machine-readable metadata, fetch the appropriate endpoint under `https://api.github.com/repos/OWNER/REPO` when the active fetch tool supports that response format:

| Evidence | Endpoint or page |
| --- | --- |
| Description, license, default branch, archived status | Repository metadata and main page |
| Setup and stated capabilities | README and documentation |
| Architecture | Relevant files at an observed commit or tag |
| Languages | `/languages` |
| Development activity | `/commits`, `/contributors` and commit pages |
| Problems and planned changes | `/issues`, `/pulls` and linked discussions |
| Released behavior | `/releases`, release notes and tags |

Follow actual pagination links when needed. Identify truncated results and API limits. If fetch cannot read JSON or authentication is required, use the corresponding public GitHub pages or a configured tool; do not fabricate statistics or treat an access failure as an empty repository.

## Investigate

Search from broad overview to specific architecture, competing implementations and reported failures. Prefer source, tests, official documentation and merged changes. Treat issues, pull requests and roadmap statements according to their actual status. Pin source links to the observed version for claims about code behavior.

For large independent questions, use available subagent tools within their current limits. Give each task a bounded question and the relevant repository/version. Combine findings after verifying the cited evidence.

## Deliver

Use [the report template](assets/report_template.md) when a full report is requested. Adapt its length to the question. Include an executive summary, architecture or timeline where useful, key findings, limitations, and sources. Separate implemented behavior from proposals and speculation; record when live metrics were observed. Save the report to the user's requested output location and link the actual file.
