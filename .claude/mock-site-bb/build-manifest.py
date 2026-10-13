import json, os
SITE = os.path.dirname(os.path.abspath(__file__))
sets = [
  ("bb-elements",   "assets/hud",     "assets/hud/set.json",               "palette_lock element/frame"),
  ("bb-crew",       "assets/crew",    "assets/crew/set.json",              "palette_lock npc variants+walk4"),
  ("bb-board",      "assets/board",   "assets/board/set.json",             "palette_lock kit/datasheet"),
  ("bb-widgets",    "assets/widgets", "assets/widgets/set.json",           "palette_lock bar/atlas/element"),
  ("code-rain",     "assets/gif",     "assets/gif/set-code-rain.json",     "from_spec gif loop (anim of hero)"),
  ("circuit-pulse", "assets/gif",     "assets/gif/set-circuit-pulse.json", "from_spec gif loop (honors bg)"),
  ("hero-banner",   "assets/gif",     "assets/gif/set-hero-banner.json",   "composite gif (job-ref anim, set-file statics)"),
]
out = {"server": "http://127.0.0.1:9106", "generated": "2026-10-13", "sets": []}
for n, d, sp, desc in sets:
    sj = json.load(open(os.path.join(SITE, sp)))
    out["sets"].append({"name": n, "set_id": sj.get("set_id"), "job_id": sj.get("job_id"),
                        "description": desc,
                        "files": [f["name"] for f in sj.get("files", [])]})
json.dump(out, open(os.path.join(SITE, "assets", "manifest.json"), "w"), indent=2)
print(json.dumps([{"name": s["name"], "set_id": s["set_id"], "files": len(s["files"])} for s in out["sets"]]))