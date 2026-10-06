#!/usr/bin/env python3
"""Second training-venv patch: keep MLX buffer cache in check.

DreamBooth's dense backward steps churn multi-GB transient buffers; MLX's
metal allocator retains them as cache and the run grows into swap on a
32 GB machine. Two hooks: clear the cache once after model init (drops the
bf16 load-phase buffers before the graph builds) and every plot tick during
training (caps steady-state growth). Clearing the cache does not discard
compiled functions or live arrays, only cached free buffers.

Superseded in the plot branch by 04-clear-cache-step.py, which clears every
step instead. Re-running this patch after 04 fails on its own assert.

Usage: python3 02-clear-cache-init.py <venv-site-packages>
"""
import sys
from pathlib import Path

sp = Path(sys.argv[1])

init = sp / "mflux/models/flux/variants/dreambooth/dreambooth_initializer.py"
text = init.read_text()
old = """        # Create and apply the LoRA layers directly to the transformer
        LoRALayers.from_spec(flux=flux, training_spec=training_spec)"""
new = """        # Create and apply the LoRA layers directly to the transformer
        LoRALayers.from_spec(flux=flux, training_spec=training_spec)

        # Release the bf16 load-phase buffers; the graph allocates fresh ones
        import mlx.core as mx
        mx.metal.clear_cache()"""
assert old in text, "initializer LoRALayers hook not found"
text = text.replace(old, new)
init.write_text(text)

db = sp / "mflux/models/flux/variants/dreambooth/dreambooth.py"
text = db.read_text()
old_plot = """            if training_state.should_plot_loss(training_spec):
                validation_batch = training_state.iterator.get_validation_batch()"""
new_plot = """            if training_state.should_plot_loss(training_spec):
                mx.metal.clear_cache()
                validation_batch = training_state.iterator.get_validation_batch()"""
assert old_plot in text, "train loop plot hook not found"
assert "from mlx import nn" in text
text = text.replace("from mlx import nn", "import mlx.core as mx\nfrom mlx import nn", 1)
text = text.replace(old_plot, new_plot)
db.write_text(text)

print("patched:", init)
print("patched:", db)