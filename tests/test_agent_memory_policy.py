from unittest.mock import patch

from deerflow.agents.lead_agent.prompt import apply_prompt_template


def test_stateless_agent_prompt_does_not_read_or_inject_memory() -> None:
    with patch(
        "deerflow.agents.lead_agent.prompt._get_memory_context",
        return_value="<memory>secret</memory>",
    ) as get_memory:
        prompt = apply_prompt_template(memory_enabled=False)

    get_memory.assert_not_called()
    assert "<memory>secret</memory>" not in prompt
