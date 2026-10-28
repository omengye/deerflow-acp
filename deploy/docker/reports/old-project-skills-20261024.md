# Gentoo 旧项目技能清单

检查日期：2026-10-24（Asia/Shanghai）

项目：`/home/gen/Documents/deerflow-api`；配置的技能路径：`./skills`。

共 85 个正式目录技能：公共 22 个、自定义 63 个；历史快照和演化候选未计入。当前容器有 25 个，其中启用 24 个；双方同名 17 个，旧目录独有 68 个。

这是文件与名称清单，未逐项验证旧技能的运行依赖、登录状态或外部接口可用性。

## 公共技能（22）

| 技能 | 当前容器 | 原技能说明 |
|---|---|---|
| `academic-paper-review` | 已有同名技能 | Use this skill when the user requests to review, analyze, critique, or summarize academic papers, research articles, preprints, or scientific publications. Supports comprehensive s |
| `bootstrap` | 已有同名技能 | - Generate a personalized SOUL.md through a warm, adaptive onboarding conversation. Trigger when the user wants to create, set up, or initialize their AI partner's identity — e.g., |
| `chart-visualization` | 已有同名技能 | This skill should be used when the user wants to visualize data. It intelligently selects the most suitable chart type from 26 available options, extracts parameters based on detai |
| `claude-to-deerflow` | 已有同名技能 | "Interact with DeerFlow AI agent platform via its HTTP API. Use this skill when the user wants to send messages or questions to DeerFlow for research/analysis, start a DeerFlow con |
| `code-documentation` | 已有同名技能 | Use this skill when the user requests to generate, create, or improve documentation for code, APIs, libraries, repositories, or software projects. Supports README generation, API r |
| `consulting-analysis` | 已有同名技能 | Use this skill when the user requests to generate, create, or write professional research reports including but not limited to market analysis, consumer insights, brand analysis, f |
| `data-analysis` | 未包含 | Use this skill when the user uploads Excel (.xlsx/.xls) or CSV files and wants to perform data analysis, generate statistics, create summaries, pivot tables, SQL queries, or any fo |
| `deep-research` | 已有同名技能 | Use this skill instead of WebSearch for ANY question requiring web research. Trigger on queries like "what is X", "explain X", "compare X and Y", "research X", or before content ge |
| `find-skills` | 已有同名技能 | Helps users discover and install agent skills when they ask questions like "how do I do X", "find a skill for X", "is there a skill that can...", or express interest in extending c |
| `frontend-design` | 已有同名技能 | Create distinctive, production-grade frontend interfaces with high design quality. Use this skill when the user asks to build web components, pages, artifacts, posters, or applicat |
| `github-deep-research` | 已有同名技能 | Conduct multi-round deep research on any GitHub Repo. Use when users request comprehensive analysis, timeline reconstruction, competitive analysis, or in-depth investigation of Git |
| `image-generation` | 未包含 | Use this skill when the user requests to generate, create, imagine, or visualize images including characters, scenes, products, or any visual content. Supports structured prompts a |
| `newsletter-generation` | 已有同名技能 | Use this skill when the user requests to generate, create, write, or draft a newsletter, email digest, weekly roundup, industry briefing, or curated content summary. Supports topic |
| `podcast-generation` | 未包含 | Use this skill when the user requests to generate, create, or produce podcasts from text content. Converts written content into a two-host conversational podcast audio format with  |
| `ppt-generation` | 未包含 | Use this skill when the user requests to generate, create, or make presentations (PPT/PPTX). Creates visually rich slides by generating images for each slide and composing them int |
| `skill-creator` | 已有同名技能 | Create new skills, modify and improve existing skills, and measure skill performance. Use when users want to create a skill from scratch, edit, or optimize an existing skill, run e |
| `surprise-me` | 已有同名技能 | Create a delightful, unexpected "wow" experience for the user by dynamically discovering and creatively combining other enabled skills. Triggers when the user says "surprise me" or |
| `systematic-literature-review` | 已有同名技能 | Use this skill when the user wants a systematic literature review, survey, or synthesis across multiple academic papers on a topic. Also covers annotated bibliographies and cross-p |
| `vercel-deploy` | 已有同名技能 | Deploy applications and websites to Vercel. Use this skill when the user requests deployment actions such as "Deploy my app", "Deploy this to production", "Create a preview deploym |
| `video-generation` | 未包含 | Use this skill when the user requests to generate, create, or imagine videos. Supports structured prompts and reference image for guided generation. |
| `weather-cma` | 已有同名技能 | "Use this skill for ANY weather query about Chinese cities — forecasts, current conditions, rain checks, temperature, weekend weather, etc. Uses China Meteorological Administration |
| `web-design-guidelines` | 已有同名技能 | Review UI code for Web Interface Guidelines compliance. Use when asked to "review my UI", "check accessibility", "audit design", "review UX", or "check my site against best practic |

## 自定义技能（63）

| 技能 | 当前容器 | 原技能说明 |
|---|---|---|
| `agently-email-sender` | 未包含 | Guidance for sending HTML email reports via agently-cli, including authentication status check, token refresh, inline HTML formatting, and non-interactive two-phase confirmation to |
| `agently-mail` | 未包含 | 通过 agently-cli 命令行工具操作邮件：发送、回复、转发、搜索、读取、下载附件、管理收件箱。当用户需要进行任何邮件相关操作时使用此 skill。 |
| `agently-mail-send` | 未包含 | Send HTML email using agently-cli (Tencent QQMail agent CLI). Handles authentication check, token refresh, and two-phase confirmation. Use when the user wants to send an email via  |
| `ai-hot-daily-briefing` | 未包含 | 获取当天最新 AI 日报资讯，整理为中文 Markdown 简报，转换为带内联样式的 HTML 邮件正文，并通过 agently-cli 发送邮件。适用于每天重复的 AI 热点日报汇总与邮件推送任务。 |
| `ai-hot-daily-email` | 未包含 | Create a Chinese AI HOT daily briefing from AI HOT news, convert it to a styled HTML email, and send it to a specified recipient using the agently-cli two-phase send flow. Use for  |
| `ai-hot-daily-report-email` | 未包含 | 获取 AI HOT 每日 AI 日报，整理成中文 Markdown 简报并输出，再转换为带内联样式的 HTML 邮件正文，通过 agently-cli 发送到指定邮箱。适用于用户要求“AI HOT 日报”或定时发送 AI 日报邮件的场景。 |
| `aihot` | 未包含 | AI HOT (aihot.virxact.com) 中文 AI 资讯查询 Skill。当用户想知道"今天 AI 圈有什么"、"AI 日报"、"AI HOT"、"AI 资讯"、"AI 热点"、"最近 AI"、"OpenAI/Anthropic/Google 最近发布了什么"、"AI hot today"、"AI news today"、"看一下 AI 行业动 |
| `aihot-daily-brief` | 未包含 | Compile and email the AI HOT daily digest in Chinese. Use when the user asks for today's AI news or daily AI report, or when a scheduled job requests an AI HOT 日报 email. Produces a |
| `aihot-daily-digest` | 未包含 | Generate a Chinese AI HOT daily brief from aihot, convert it to inline-styled HTML, and send it via agently-cli with two-phase confirmation. Use for daily recurring AI news digest  |
| `aihot-daily-digest-email` | 未包含 | Generate a Chinese AI HOT daily news digest in Markdown, convert it to a styled HTML email, and send it via agently-cli using the non-interactive two-phase confirmation flow. Use f |
| `aihot-daily-email` | 未包含 | 发送 AI HOT 日报邮件。当用户要求"发送AI日报邮件"、"把AI日报发我邮箱"、"给我发下AI日报"或类似请求时，综合使用 aihot 和 baoyu-markdown-to-html 技能拉取日报、转换为格式良好的邮件并发送到指定收件人。 |
| `baoyu-article-illustrator` | 未包含 | Analyzes article structure, identifies positions requiring visual aids, generates illustrations with Type × Style × Palette three-dimension approach. Use when user asks to "illustr |
| `baoyu-comic` | 未包含 | Knowledge comic creator supporting multiple art styles and tones. Creates original educational comics with detailed panel layouts and batch-capable image generation. Use when user  |
| `baoyu-compress-image` | 未包含 | Compresses images to WebP (default) or PNG with automatic tool selection. Use when user asks to "compress image", "optimize image", "convert to webp", or reduce image file size. |
| `baoyu-cover-image` | 未包含 | Generates article cover images with 5 dimensions (type, palette, rendering, text, mood) combining 11 color palettes and 7 rendering styles. Supports cinematic (2.35:1), widescreen  |
| `baoyu-danger-gemini-web` | 未包含 | Generates images and text via reverse-engineered Gemini Web API. Supports text generation, image generation from prompts, reference images for vision input, and multi-turn conversa |
| `baoyu-danger-x-to-markdown` | 未包含 | Converts X (Twitter) tweets and articles to markdown with YAML front matter. Uses reverse-engineered API requiring user consent. Use when user mentions "X to markdown", "tweet to m |
| `baoyu-diagram` | 未包含 | Create professional, dark-themed SVG diagrams of any type — architecture diagrams, flowcharts, sequence diagrams, structural diagrams, mind maps, timelines, illustrative/conceptual |
| `baoyu-electron-extract` | 未包含 | Extracts resources and JavaScript from any installed Electron app (`.asar` bundle), restoring original sources from `.js.map` files when available or formatting minified code with  |
| `baoyu-format-markdown` | 未包含 | Formats plain text or markdown files with frontmatter, titles, summaries, headings, bold, lists, and code blocks. Use when user asks to "format markdown", "beautify article", "add  |
| `baoyu-image-gen` | 未包含 | AI image generation with OpenAI GPT Image 2, Azure OpenAI, Google, OpenRouter, DashScope, Z.AI GLM-Image, MiniMax, Jimeng, Seedream and Replicate APIs. Supports text-to-image, refe |
| `baoyu-infographic` | 未包含 | Generate professional infographics with 21 layout types and 22 visual styles. Analyzes content, recommends layout×style combinations, and generates publication-ready infographics.  |
| `baoyu-markdown-to-html` | 未包含 | Converts Markdown to styled HTML with WeChat-compatible themes. Supports code highlighting, math, Mermaid (rendered to PNG via headless Chrome), PlantUML, footnotes, alerts, infogr |
| `baoyu-post-to-wechat` | 未包含 | Posts content to WeChat Official Account (微信公众号) via API or Chrome CDP. Supports article posting (文章) with HTML, markdown, or plain text input, and image-text posting (贴图, formerly |
| `baoyu-post-to-weibo` | 未包含 | Posts content to Weibo (微博). Supports regular posts with text, images, and videos, and headline articles (头条文章) with Markdown input via Chrome CDP. Use when user asks to "post to W |
| `baoyu-post-to-x` | 未包含 | Posts content and articles to X (Twitter). Supports regular posts with images/videos and X Articles (long-form Markdown). In Codex, honor explicit requests for the Codex Chrome plu |
| `baoyu-slide-deck` | 未包含 | Generates professional slide deck images from content. Creates outlines with style instructions, then generates individual slide images. Use when user asks to "create slides", "mak |
| `baoyu-translate` | 未包含 | Translates articles and documents between languages with three modes - quick (direct), normal (analyze then translate), and refined (analyze, translate, review, polish). Supports c |
| `baoyu-url-to-markdown` | 未包含 | Fetch any URL and convert to markdown using baoyu-fetch CLI (Chrome CDP with site-specific adapters). Built-in adapters for X/Twitter, YouTube transcripts, Hacker News threads, and |
| `baoyu-wechat-summary` | 未包含 | Summarizes WeChat group chat highlights into a structured digest using the local wx-cli binary (https://github.com/jackwener/wx-cli). Generates a normal digest by default; a roast  |
| `baoyu-xhs-images` | 未包含 | Generates infographic image card series with 12 visual styles, 8 layouts, and 3 color palettes. Breaks content into 1-10 cartoon-style image cards optimized for social media engage |
| `baoyu-youtube-transcript` | 未包含 | Downloads YouTube video transcripts/subtitles and cover images by URL or video ID. Supports multiple languages, translation, chapters, and speaker identification. Caches raw data f |
| `bilibili-trending` | 未包含 | Fetch Bilibili (B站) trending videos, ranking boards, user following feed, and single-video analysis (metadata, UP主 upload history, comments) using the host_opencli tool. Includes e |
| `check-open-source-commercial-license` | 未包含 | Determine if a software project can be used commercially by inspecting its open-source license, separate asset licenses, and trademark terms. Use this when the user asks "Can I use |
| `community-notification-triage` | 未包含 | Triage unread community notifications by separating passive reactions (likes/upvotes) from substantive comments, replying to comments that warrant engagement, marking notifications |
| `daily-ai-hn-digest` | 未包含 | Generate and deliver the user's recurring daily AI HOT report and Hacker News hot distribution digest. |
| `diagnose-scheduled-tasks` | 未包含 | Diagnose missing or empty scheduled tasks by systematically checking application-specific task lists, system crontab, systemd timers, API endpoints, and filesystem artifacts. |
| `fictional-plot-research` | 未包含 | Use when the user asks for detailed explanations of events, battles, or character actions in a fictional narrative (novel, manga, game, movie, etc.) that require external research. |
| `github-issue-research` | 未包含 | Research GitHub issues, pull requests, and code markers to determine whether a feature is implemented, planned, or unsupported. |
| `github-project-feature-research` | 未包含 | Verify whether an open-source GitHub project supports a specific feature. Use when a user asks about a project's capability, configuration options, or protocol support. Covers web  |
| `hackernews-digest` | 未包含 | Hacker News 每日热门摘要 Skill。当用户想要查看、整理或发布 Hacker News 前 10 热门帖子时使用。支持抓取 HN 首页、生成中文摘要、发布到 InStreet 社区、发送邮件及同步至 Notion。触发关键词：Hacker News、HN、HN digest、HN 热门、HN 前10、hackernews、今天的 HN。 |
| `hn-daily-digest` | 未包含 | Create and distribute a daily Hacker News top-10 digest to InStreet, Notion, and email. Trigger when the user asks to continue the HN digest workflow or when a recurring daily dist |
| `hn-digest-distribution` | 未包含 | Build and distribute a daily Chinese-language Hacker News top-10 digest across InStreet, Notion, and email. Use when the user asks to send or publish today's HN digest, or confirms |
| `instreet` | 未包含 | InStreet AI Agent 社交网络操作 Skill。当用户需要访问 instreet.coze.site 社区、查看帖子、发帖、评论、点赞、管理小组、参与竞技场/文学社/预言机/桌游室、查看 Agent 动态、搜索用户或进行任何 InStreet 相关操作时使用此 skill。触发关键词：instreet、InStreet、AI Agent 社区、 |
| `instreet-daily-post-check` | 未包含 | Verify and summarize the user's InStreet submissions for a given day, including post data and comments. |
| `lore-provenance-research` | 未包含 | Use when the user asks how a character, item, or ability in a fictional work was obtained, or asks for a more concrete origin chain ("怎么得到的？具体一点"). Produces a verified, chronologic |
| `multi-source-research-synthesis` | 未包含 | Use for complex factual, biographical, or narrative questions that require gathering information from multiple web searches and synthesizing a cited answer. Especially useful when  |
| `my-coffee` | 未包含 | Use when users ask to order Luckin Coffee, search Luckin stores/products, query pickup code/order status, cancel a Luckin order, or mention 瑞幸、luckin、咖啡、点单、下单、门店、取餐码. |
| `notion-api` | 未包含 | This skill provides comprehensive instructions for interacting with the Notion API via REST calls. This skill should be used whenever the user asks to interact with Notion, includi |
| `open-source-project-evaluation` | 未包含 | Evaluate the capabilities of an open-source AI agent project by researching official docs, GitHub issues, and real-user feedback, then produce a structured assessment. Use when the |
| `opencli` | 未包含 | OpenCLI 多站点适配器工具。通过 host_opencli 工具操作小红书 (xiaohongshu)、抖音 (douyin)、Twitter (twitter)、GitHub (github) 等站点。当用户需要进行浏览、搜索、获取内容、查看用户等操作时使用此 skill。触发关键词：小红书、抖音、opencli、host_opencli、浏览抖音、 |
| `qq-music` | 未包含 | QQ Music search, parse, and download workflow using api.nki.pw. Use when the user asks to search QQ音乐/QQ Music songs, resolve downloadable audio URLs, download a song by keyword or |
| `rain-tomorrow-check` | 未包含 | Answer whether it will rain tomorrow, including rain type, temperature, precipitation timing, and an umbrella suggestion. Use when the user asks 明天下雨吗 or similar repeated rain ques |
| `sandbox-init` | 未包含 | Sandbox HOME directory readiness probe. Run at the start of every new conversation session, before any other work, to avoid a race condition where /etc/profile.d/nodejs.sh runs bef |
| `sheet-music-to-midi` | 未包含 | "Professional sheet music to MIDI converter. Supports: (1) sheet music images (JPG/PNG) → MIDI via Optical Music Recognition (OMR), (2) MusicXML/MXL → MIDI, (3) ABC notation → MIDI |
| `soda-music` | 未包含 | 汽水音乐（Soda Music）搜索与下载 Skill。当用户需要搜索汽水音乐歌曲、查看红心歌单、获取歌单曲目、下载/解密歌曲时使用。触发关键词：汽水音乐、soda music、汽水、soda、抖音音乐。 |
| `song-download` | 未包含 | 搜索并下载歌曲到本地。基于酷我音乐 API（西瓜糖 API），支持关键词搜索、交互式选歌、指定音质、可选转码为 24kHz mono 96kbps MP3。不依赖任何硬件，纯粹的"搜歌、下歌、存歌"。 |
| `subway-seat-tracker` | 未包含 | "自动追踪地铁座位选择结果（选对/选错）及详细心得过程，追加到本地日志文件并同步更新 Notion 页面。当用户提到‘地铁选对’、‘地铁选错’、‘地铁座位’、‘地铁胜率’、‘选座记录’、‘地铁打卡’等相关词汇时触发。" |
| `toy-playmusic` | 未包含 | Play an existing local music/audio file, or a music file selected from a specified local directory, on a FoloToy toy by calling the host sidecar local playback API when available,  |
| `video-insights` | 未包含 | Extract and summarize key insights from a video link, with optional contextual research, while minimizing tool usage. |
| `windows-acp-toolchain-diagnosis` | 未包含 | Diagnose why a Windows ACP tool (e.g., remote_deerflow) is unavailable or failing. Use when users ask about Windows ACP tools, remote_deerflow connectivity, missing ACP bridge/daem |
| `x-trending-topics` | 未包含 | Retrieve and summarize current trending topics on X (formerly Twitter). Use when the user asks what is trending on X, or asks for hot/popular topics on X. |
| `xianxia-lore-research` | 未包含 | Research and explain cultivation techniques, artifacts, characters, or power systems in Chinese xianxia/xuanhuan web novels, especially "A Record of a Mortal's Journey to Immortali |

## 当前容器独有的技能

`deepseek-search`、`fanren-rag`、`huaweicloud-linux`、`huaweicloud-sandbox`、`instabox-exec-windows`、`searxng-web-search`、`show-me`、`ulw-plan`
