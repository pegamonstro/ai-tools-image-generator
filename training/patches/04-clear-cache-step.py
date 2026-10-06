#!/usr/bin/env python3
"""Fourth training-venv patch: clear the MLX buffer cache every step.

The plot-tick cadence (20 steps) still let per-step transient buffers
accumulate into multiple GB between clears, driving the process into
swap (44 GB in a 32 GB machine). Move the clear to every training step:
cached buffers are freed before the next step's allocation, so steady
state stays near the quantized model plus a single step's transients.
Requires 03-force-eval.py to have been applied (its block is the anchor).
Also reverts the plot-tick hook introduced by 02-clear-cache-init.py.

Usage: python3 04-clear-cache-step.py <venv-site-packages>
"""
import sys
from pathlib import Path

sp = Path(sys.argv[1])

db = sp / "mflux/models/flux/variants/dreambooth/dreambooth.py"
text = db.read_text()

old = """            loss, grads = train_step_function(batch)
            training_state.optimizer.optimizer.update(model=flux, gradients=grads)
            mx.eval(loss, flux.parameters(), training_state.optimizer.optimizer.state)
            batches.set_postfix(loss=f"{float(loss):.4f}")
            del loss, grads"""
new = """            loss, grads = train_step_function(batch)
            training_state.optimizer.optimizer.update(model=flux, gradients=grads)
            mx.eval(loss, grads, flux.parameters(), training_state.optimizer.optimizer.state)
            batches.set_postfix(loss=f"{float(loss):.4f}")
            del loss, grads
            mx.metal.clear_cache()"""
assert old in text, "train step block not found"
assert text.count(old) == 1
text = text.replace(old, new)

plot_old = """            if training_state.should_plot_loss(training_spec):
                mx.metal.clear_cache()
                validation_batch = training_state.iterator.get_validation_batch()"""
plot_new = """            if training_state.should_plot_loss(training_spec):
                validation_batch = training_state.iterator.get_validation_batch()"""
assert plot_old in text, "plot hook not found (02 applied?)"
text = text.replace(plot_old, plot_new)
db.write_text(text)

print("patched:", db)