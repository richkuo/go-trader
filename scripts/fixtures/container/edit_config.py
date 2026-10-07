import json
import sys

path = sys.argv[1]
with open(path) as f:
    cfg = json.load(f)
for item in sys.argv[2:]:
    key, _, raw = item.partition("=")
    if raw == "__delete__":
        cfg.pop(key, None)
        continue
    value = json.loads(raw)
    if key == "strategies+":
        cfg["strategies"].append(value)
    elif key.startswith("strategies[0]."):
        cfg["strategies"][0][key.split(".", 1)[1]] = value
    else:
        cfg[key] = value
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
