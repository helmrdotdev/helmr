import json
import sys

import numpy as np

with open(sys.argv[1]) as source:
    data = json.load(source)

samples = np.array(data["samples"], dtype=np.float64)
print("loaded", len(samples), flush=True)
solution = np.linalg.solve(np.array(data["matrix"], dtype=np.float64), np.array(data["vector"], dtype=np.float64))
print("solved", flush=True)

with open(sys.argv[2], "w") as target:
    json.dump({
        "numpy": np.__version__,
        "mean": float(samples.mean()),
        "std": float(samples.std()),
        "solution": [round(float(value), 9) for value in solution],
    }, target)
