# Load-test corpus (M19)

The scenario and definition corpus the load campaign runs on. The methodology,
SLOs, and pre-registered hypotheses are in [`docs/load/plan.md`](../../docs/load/plan.md).

```
definitions/   workflow definitions (offline on the mock provider)
  linear_10.json     10-step sequential chain          — H1 (serial throughput)
  fanout_50.json     seed → 50 parallel → join → final — H2/H4/H6
  planner_heavy.json 2 sequential runtime expansions    — H2 (expansion write amp)
  agent_loop.json    writer⇄critic, 3 loop-backs        — loop unrolling under load
scenarios/     named load configs (arrival profile + SLO targets)
  linear-10.json  fanout-50.json  planner-heavy.json  agent-loop.json  mixed.json
mock.json      the fleet mock script: lognormal latency + token distributions +
               the always-revise critic rule agent-loop needs
```

- **Definitions** are validated (decode + full `dag.Validate`) by
  `internal/loadtest`'s `TestScenarioCorpusLoads`, so a malformed fixture fails
  CI before any load run.
- **Scenarios** are parsed and cross-checked (definition exists, mix entries
  resolve) by the same package — "runnable as named configs" is a unit test,
  not a promise. The load generator (`cmd/loadgen`, ticket 19.2) consumes the
  same parser.
- **The mock script** is mounted into every worker by `docker-compose.load.yml`
  (`AGENTLOOM_LLM_MOCK_SCRIPT_FILE`). It is parsed by `llm.ParseMockScript` +
  `llm.NewMock`; a malformed script fails worker boot.

Boot the pinned environment with `make load-up` (see the plan for pins and the
one-command details).
