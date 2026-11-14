# Harness behavior evals

Each `*.json` file here is one end-to-end scenario run by `TestBehaviorEval`
(`internal/engine/eino/behavior_eval_test.go`) through the production runtime
path: durable SQLite sessions, checkpoints, budget ledger and asset store.
The model is scripted and tools are fixtures, so runs are deterministic and need
no network or credentials.

CI runs the set as a named gate:

```sh
go test ./internal/engine/eino -run '^TestBehaviorEval$' -count=1
```

The gate also fails if the set shrinks below 20 scenarios.

## Scenario shape

```jsonc
{
  "name": "unique-kebab-name",          // must match the file name by convention
  "category": "resilience",             // basic, recovery, loop-guard, long-task, context,
                                        // budget, permissions, interruption, resilience
  "description": "What the harness must do, stated as behavior.",
  "budget": {"maxModelCalls": 40, "maxToolCalls": 40},   // optional
  "compaction": {"enabled": true, "keepRecentMessages": 1}, // optional
  "permissionMode": "all",              // optional; "all" asks for every tool
  "tools": [{"name": "build", "results": [
    {"output": "ok"},                   // results are served in order,
    {"error": "exit status 2"},         // the last one repeats
    {"output": "x", "repeatOutput": 100000},
    {"hang": true}                      // blocks until cancelled or crashed
  ]}],
  "model": [                            // one step per root model call
    {"toolCalls": [{"name": "build"}], "repeat": 3},
    {"error": {"status": 503}},
    {"text": "done", "turn": 1, "optional": false,
     "expectInput": {"contains": ["ok"], "absent": [], "systemContains": [],
                     "closedHistory": true, "maxBytes": 20000}}
  ],
  "turns": [
    {"prompt": "Do the task.", "permission": "allow",
     "expect": {"stopReason": "end_turn", "error": "", "limit": "",
                "textContains": [], "eventKinds": [], "permissions": 1,
                "executionStatus": "waiting_input"}},
    {"action": "resume"}                // also: restart, cancelDuringTool, crashDuringTool
  ],
  "expect": {"toolRuns": {"build": 1}, "modelCalls": 2, "maxModelCalls": 10, "summaries": 1}
}
```

Unknown fields are rejected. Model steps pinned with `turn` are served only in
that turn; `optional` steps may stay unused (for loops whose length the harness
decides). Summarization calls are answered automatically and not counted.

Write expectations as user-visible outcomes (stop reason, effects executed,
what the model saw), not implementation details, so scenarios survive refactors.
