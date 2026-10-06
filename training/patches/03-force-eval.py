#!/usr/bin/env python3
"""Third training-venv patch: force MLX evaluation every training step.

The dreambooth loop builds a lazy graph per step (loss, grads, optimizer
update) without ever calling mx.eval, so under recent MLX (0.30.x) the
deferred graph accumulates unbounded and materializes in one giant spike
~18 steps in, thrashing the machine into swap. Force per-step evaluation
of the loss, model parameters, and optimizer state, and surface the loss
value on the progress bar.

Usage: python3 03-force-eval.py <venv-site-packages>
"""
import sys
from pathlib import Path

sp = Path(sys.argv[1])

db = sp / "mflux/models/flux/variants/dreambooth/dreambooth.py"
text = db.read_text()

old = """            loss, grads = train_step_function(batch)
            training_state.optimizer.optimizer.update(model=flux, gradients=grads)
            del loss, grads"""
new = """            loss, grads = train_step_function(batch)
            training_state.optimizer.optimizer.update(model=flux, gradients=grads)
            mx.eval(loss, flux.parameters(), training_state.optimizer.optimizer.state)
            batches.set_postfix(loss=f"{float(loss):.4f}")
            del loss, grads"""
assert old in text, "train step block not found"
assert text.count(old) == 1
text = text.replace(old, new)
db.write_text(text)

print("patched:", db)