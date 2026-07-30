"""Render the immutable v1 agent prompt artifacts."""

from pathlib import Path

from harness_agents.prompt_protocol import render_all_prompts

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "python" / "prompts"


def main() -> None:
    OUTPUT.mkdir(parents=True, exist_ok=True)
    for stage, content in render_all_prompts().items():
        (OUTPUT / f"{stage}.md").write_bytes(content)


if __name__ == "__main__":
    main()
