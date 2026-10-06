#!/usr/bin/env python3
"""Fifth training-venv patch: cut the dev config's T5 max_sequence_length.

Flux pads every caption to the T5 tokenizer's max_length, so the 512-token
default doubles the transformer's joint attention sequence even though the
training captions are ~35 tokens. 128 tokens is ample for this dataset and
shrinks every training step's attention work accordingly.

Usage: python3 05-t5-max-seq.py <venv-site-packages>
"""
import sys
from pathlib import Path

sp = Path(sys.argv[1])

mc = sp / "mflux/models/common/config/model_config.py"
text = mc.read_text()

old = """    "dev": ModelConfig(
        priority=0,
        aliases=["dev"],
        model_name="black-forest-labs/FLUX.1-dev",
        base_model=None,
        controlnet_model=None,
        custom_transformer_model=None,
        num_train_steps=1000,
        max_sequence_length=512,"""
new = old.replace("max_sequence_length=512,", "max_sequence_length=128,")
assert old in text, "dev entry not found"
assert text.count(old) == 1
text = text.replace(old, new)
mc.write_text(text)
print("patched:", mc)