#!/usr/bin/env python3
"""Patch the flux-train venv's DreamBooth to load a local model_path.

mflux 0.15.5 (PyPI) dropped model_path from DreamBoothInitializer while Flux1
still accepts it. This restores the 0.20 behaviour: TrainingSpec gains an
optional model_path, and the initializer threads it into Flux1 so a
local fine-tune dir can be trained on directly.
Training-venv-only change; the shared uv-tool mflux (generation) is untouched.

Usage: python3 01-model-path.py <venv-site-packages>
"""
import sys
from pathlib import Path

sp = Path(sys.argv[1])

spec = sp / "mflux/models/flux/variants/dreambooth/state/training_spec.py"
text = spec.read_text()

dataclass_field = (
    "    statistics: StatisticsSpec\n"
    "    examples: List[ExampleSpec]\n"
    "    config_path: str | None = None\n"
    "    checkpoint_path: str | None = None\n"
)
assert dataclass_field in text, "TrainingSpec fields block not found"
text = text.replace(
    dataclass_field,
    dataclass_field + "    model_path: str | None = None\n",
)

config_anchor = (
    "            config_path=None if absolute_config_path is None else str(absolute_config_path),\n"
    "        )"
)
assert config_anchor in text, "from_conf return tail not found"
text = text.replace(
    config_anchor,
    '            config_path=None if absolute_config_path is None else str(absolute_config_path),\n'
    '            model_path=config.get("model_path"),\n'
    "        )",
)
spec.write_text(text)

init = sp / "mflux/models/flux/variants/dreambooth/dreambooth_initializer.py"
text = init.read_text()
old = """        model_config = ModelConfig.from_name(training_spec.model)
        flux = Flux1(
            model_config=model_config,
            quantize=training_spec.quantize,
        )"""
new = """        model_config = ModelConfig.from_name(training_spec.model)
        flux = Flux1(
            model_config=model_config,
            quantize=training_spec.quantize,
            model_path=training_spec.model_path,
        )"""
assert old in text, "initializer Flux1 construction not found"
text = text.replace(old, new)
init.write_text(text)

print("patched:", spec)
print("patched:", init)