#!/usr/bin/env python3
"""Prepare a img-gen LoRA training dataset from a folder of source images.

Stdlib-only. Copies images into training/datasets/<slug>/, writes and
validates .txt caption sidecars (mflux-train auto-discovers image+txt pairs),
checks dimensions, prints a manifest, and (optionally) fills the
mflux-train.json.example template for the new dataset.

    python3 training/prepare_dataset.py --src ~/Downloads/aria-set \
        --dataset aria --trigger aria --captions ~/Downloads/aria-set/captions.txt \
        --write-config
"""

import argparse
import json
import shutil
import struct
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DATASETS = REPO / "training" / "datasets"
IMG_EXTS = {".jpg", ".jpeg", ".png", ".webp"}


def sniff_dims(path: Path):
    """Return (width, height) for JPEG/JFIF and PNG without decoders."""
    with open(path, "rb") as f:
        data = f.read(2 ** 20)
    if data[:8] == b"\x89PNG\r\n\x1a\n" and data[12:16] == b"IHDR":
        w, h = struct.unpack(">II", data[16:24])
        return int(w), int(h)
    if data[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(data):
            if data[i] != 0xFF:
                i += 1
                continue
            marker = data[i + 1]
            if 0xD8 <= marker <= 0xD9 or marker == 0x01:
                i += 2
                continue
            (seg_len,) = struct.unpack(">H", data[i + 2 : i + 4])
            if 0xC0 <= marker <= 0xCF and marker not in (0xC4, 0xC8, 0xCC):
                h, w = struct.unpack(">HH", data[i + 5 : i + 9])
                return int(w), int(h)
            i += 2 + seg_len
    return None


def load_caption_map(path: Path) -> dict:
    """captions.txt lines: '<stem><TAB><caption>'."""
    by_stem = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.rstrip()
        if not line or line.lstrip().startswith("#"):
            continue
        stem, sep, caption = line.partition("\t")
        if not sep:
            stem, sep, caption = line.partition(" ")
        if not sep or not caption.strip():
            raise SystemExit(f"{path}: caption lines must be '<stem><TAB><caption>': {line!r}")
        by_stem[stem.strip()] = caption.strip()
    return by_stem


def write_config(slug: str, template: Path, out: Path) -> None:
    cfg = json.loads(template.read_text())
    cfg.pop("_comment", None)
    dataset = DATASETS / slug
    images = []
    for p in sorted(dataset.iterdir()):
        if p.suffix.lower() not in IMG_EXTS:
            continue
        sidecar = dataset / (p.stem + ".txt")
        caption = sidecar.read_text(encoding="utf-8").strip()
        images.append({"image": p.name, "prompt": caption})
    if not images:
        raise SystemExit(f"no captioned images in {dataset}")
    cfg["examples"] = {"path": f"datasets/{slug}/", "images": images}
    out_path = cfg.get("save", {}).get("output_path", "out/<slug>")
    if "<slug>" in out_path:
        cfg["save"]["output_path"] = out_path.replace("<slug>", slug)
    out.write_text(json.dumps(cfg, indent=2) + "\n")


def main() -> None:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--src", required=True, help="directory of source images to ingest")
    ap.add_argument("--dataset", required=True, help="dataset slug (training/datasets/<slug>)")
    ap.add_argument("--trigger", required=True, help="trigger word, prepended to every caption")
    ap.add_argument(
        "--captions",
        help="caption manifest: '<stem><TAB><caption>' lines (stem = source filename without ext)",
    )
    ap.add_argument(
        "--bootstrap-captions",
        action="store_true",
        help="write placeholder '<trigger>, a <dataset>' captions for images without one",
    )
    ap.add_argument("--write-config", action="store_true", help="fill mflux-train.json.example")
    ap.add_argument(
        "--template",
        default=str(REPO / "training" / "mflux-train.json.example"),
        help="config template used with --write-config",
    )
    ap.add_argument("--check-only", action="store_true", help="no copying or writing; report only")
    args = ap.parse_args()

    src = Path(args.src).expanduser()
    if not src.is_dir():
        raise SystemExit(f"--src is not a directory: {src}")
    sources = sorted(p for p in src.iterdir() if p.suffix.lower() in IMG_EXTS and p.is_file())
    if not sources:
        raise SystemExit(f"no images ({', '.join(sorted(IMG_EXTS))}) in {src}")

    caps = load_caption_map(Path(args.captions).expanduser()) if args.captions else {}
    unknown = sorted(set(caps) - {p.stem for p in sources})
    if unknown:
        raise SystemExit(f"caption stems not in --src: {', '.join(unknown)}")

    dest = DATASETS / args.dataset
    rows, missing, copied = [], [], 0
    for i, p in enumerate(sources):
        txt = dest / (p.stem + ".txt")
        src_txt = src / (p.stem + ".txt")
        existing_caption = txt.read_text(encoding="utf-8").strip() if txt.exists() else None
        if existing_caption is None and src_txt.exists():
            existing_caption = src_txt.read_text(encoding="utf-8").strip()
        final = existing_caption or caps.get(p.stem)
        if final is None and args.bootstrap_captions and not args.check_only:
            final = f"{args.trigger}, a {args.dataset}"
        missing.append((p, final))
        rows.append((i, p, final, sniff_dims(p)))

    print(f"dataset: {dest}")
    for i, p, final, dims in rows:
        w, h = dims or (None, None)
        small = f"  MIN-SIDE<1024 ({w}x{h})" if dims and min(w, h) < 1024 else ""
        no_trig = ""
        if final and not final.lower().startswith(args.trigger.lower()):
            no_trig = "  CAPTION-LACKS-TRIGGER"
        state = "caption" if final else "MISSING CAPTION"
        print(f"  {i:02d} {p.name:<24} {w or '?'}x{h or '?'}  {state}{small}{no_trig}")

    ok = all(final for _, final in missing)
    if not ok:
        names = ", ".join(p.name for p, final in missing if not final)
        print(f"missing captions for: {names}")
        print("edit them, re-run with --captions, or use --bootstrap-captions")
        sys.exit(1)

    if args.check_only:
        return

    dest.mkdir(parents=True, exist_ok=True)
    for i, p, final, _ in rows:
        img_dst = dest / p.name
        if not img_dst.exists():
            shutil.copy2(p, img_dst)
            copied += 1
        txt_dst = dest / (p.stem + ".txt")
        if not txt_dst.exists() and final:
            txt_dst.write_text(final + "\n", encoding="utf-8")

    if args.write_config:
        cfg_out = REPO / "training" / f"{args.dataset}.train.json"
        write_config(args.dataset, Path(args.template), cfg_out)
        print(f"wrote {cfg_out}")

    print(f"\ncopied {copied} image(s) into {dest}")
    print("\ntrain on the training host per training/README.md (patched 0.15.5 venv)")
    print(f"launch from training/ so the relative paths ({args.dataset}.train.json's")
    print("examples.path and save.output_path) resolve")


if __name__ == "__main__":
    main()