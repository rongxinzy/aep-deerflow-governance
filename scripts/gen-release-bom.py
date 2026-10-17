#!/usr/bin/env python3
"""Generate a release bill of materials (BOM) for the zhiyuan-stack chart.

Renders the chart (or reads an already rendered manifest) and lists every
container image a release deploys: pod containers/initContainers plus the
images the operator and portal hand to the pods they create (``*_IMAGE`` env
values). Each entry records repository, tag, digest (when pinned as
``repo:tag@sha256:...``) and the objects that use it.

Examples::

    # BOM for what the chart deploys with your environment values
    scripts/gen-release-bom.py -f charts/zhiyuan-stack/values-cicd.local.yaml \
        --format markdown

    # From a rendered manifest, failing if any image is not digest-pinned
    helm template zhiyuan charts/zhiyuan-stack -f v.yaml > out.yaml
    scripts/gen-release-bom.py --manifest out.yaml --require-digest

    # Look up missing digests in the registry (needs registry read access)
    scripts/gen-release-bom.py -f v.yaml --resolve

The script only reads: it never pushes, tags or deploys anything.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
from typing import Callable, Iterable

import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "charts/zhiyuan-stack"
DIGEST_RE = re.compile(r"^sha256:[a-f0-9]{64}$")
POD_KEYS = ("containers", "initContainers")


def split_image(image: str) -> dict:
    """Split ``registry/repo[:tag][@digest]`` without confusing a port for a tag."""
    rest, _, digest = image.partition("@")
    name, tag = rest, ""
    slash = rest.rfind("/")
    colon = rest.rfind(":")
    if colon > slash:
        name, tag = rest[:colon], rest[colon + 1:]
    if digest and not DIGEST_RE.match(digest):
        raise ValueError(f"invalid digest in image {image!r}")
    return {"image": image, "repository": name, "tag": tag, "digest": digest}


def pod_spec(doc: dict) -> dict:
    spec = doc.get("spec") or {}
    if doc.get("kind") == "Pod":
        return spec
    if doc.get("kind") == "CronJob":
        spec = (spec.get("jobTemplate") or {}).get("spec") or {}
    return (spec.get("template") or {}).get("spec") or {}


def iter_images(docs: Iterable[dict]):
    """Yield (image, source) for every image referenced by the manifests."""
    for doc in docs:
        if not isinstance(doc, dict) or "kind" not in doc:
            continue
        meta = doc.get("metadata") or {}
        where = f"{doc['kind']}/{meta.get('namespace', '')}/{meta.get('name', '')}".replace("//", "/")
        for key in POD_KEYS:
            for container in pod_spec(doc).get(key) or []:
                if container.get("image"):
                    yield container["image"], f"{where}:{container.get('name', '')}"
                for env in container.get("env") or []:
                    value = env.get("value")
                    if env.get("name", "").endswith("_IMAGE") and isinstance(value, str) and value:
                        yield value, f"{where}:{container.get('name', '')}:env:{env['name']}"


def collect(docs: Iterable[dict]) -> list[dict]:
    entries: dict[str, dict] = {}
    for image, source in iter_images(docs):
        entry = entries.setdefault(image, {**split_image(image), "sources": []})
        if source not in entry["sources"]:
            entry["sources"].append(source)
    for entry in entries.values():
        entry["sources"].sort()
    return sorted(entries.values(), key=lambda e: (e["repository"], e["tag"], e["digest"]))


def registry_resolver(image: str) -> str:
    """Return the registry digest of ``image`` via crane or docker buildx."""
    if shutil.which("crane"):
        command = ["crane", "digest", image]
    elif shutil.which("docker"):
        command = ["docker", "buildx", "imagetools", "inspect", image, "--format", "{{.Manifest.Digest}}"]
    else:
        raise RuntimeError("--resolve needs crane or docker buildx on PATH")
    out = subprocess.run(command, text=True, capture_output=True, check=False)
    if out.returncode != 0:
        raise RuntimeError(f"cannot resolve {image}: {out.stderr.strip()}")
    return out.stdout.strip()


def resolve(entries: list[dict], resolver: Callable[[str], str]) -> list[str]:
    """Fill missing digests in place; return the errors (resolution is best effort)."""
    errors = []
    for entry in entries:
        if entry["digest"]:
            continue
        try:
            digest = resolver(entry["image"])
        except RuntimeError as exc:
            errors.append(str(exc))
            continue
        if not DIGEST_RE.match(digest):
            errors.append(f"resolver returned {digest!r} for {entry['image']}")
            continue
        entry["digest"] = digest
        entry["resolved"] = True
    return errors


def render_chart(chart: Path, values: list[str], sets: list[str], helm: str) -> str:
    command = [helm, "template", "zhiyuan", str(chart)]
    for path in values:
        command += ["-f", path]
    for setting in sets:
        command += ["--set", setting]
    out = subprocess.run(command, text=True, capture_output=True, check=False)
    if out.returncode != 0:
        raise SystemExit(f"helm template failed:\n{out.stderr}")
    return out.stdout


def git_revision(repo: Path) -> str:
    out = subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True,
                         capture_output=True, check=False)
    return out.stdout.strip() if out.returncode == 0 else ""


def chart_meta(chart: Path) -> dict:
    data = yaml.safe_load((chart / "Chart.yaml").read_text()) or {}
    return {"name": data.get("name", ""), "version": str(data.get("version", "")),
            "appVersion": str(data.get("appVersion", ""))}


def build_bom(entries: list[dict], chart: dict, revision: str, now: dt.datetime) -> dict:
    return {
        "schema": "zhiyuan-release-bom/v1",
        "generatedAt": now.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "chart": chart,
        "gitRevision": revision,
        "images": entries,
        "summary": {"images": len(entries), "digestPinned": sum(1 for e in entries if e["digest"])},
    }


def to_markdown(bom: dict) -> str:
    chart = bom["chart"]
    lines = [
        f"# 发版 BOM: {chart['name']} {chart['version']}",
        "",
        f"- 生成时间（UTC）: {bom['generatedAt']}",
        f"- Git 版本: `{bom['gitRevision'] or '未知'}`",
        f"- 镜像: {bom['summary']['images']}，已按 digest 固定: {bom['summary']['digestPinned']}",
        "",
        "| 仓库 | tag | digest | 使用方 |",
        "| --- | --- | --- | --- |",
    ]
    for e in bom["images"]:
        digest = f"`{e['digest']}`" if e["digest"] else "⚠️ 未固定"
        lines.append(f"| `{e['repository']}` | `{e['tag'] or '-'}` | {digest} | {'<br>'.join(e['sources'])} |")
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None, resolver: Callable[[str], str] = registry_resolver) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--chart", type=Path, default=CHART)
    parser.add_argument("-f", "--values", action="append", default=[], help="values file (repeatable)")
    parser.add_argument("--set", dest="sets", action="append", default=[], help="helm --set (repeatable)")
    parser.add_argument("--manifest", help="read a rendered manifest ('-' for stdin) instead of running helm")
    parser.add_argument("--format", choices=("json", "markdown"), default="json")
    parser.add_argument("-o", "--output", help="write here instead of stdout")
    parser.add_argument("--resolve", action="store_true", help="look up missing digests in the registry")
    parser.add_argument("--require-digest", action="store_true", help="exit 1 if any image lacks a digest")
    args = parser.parse_args(argv)

    if args.manifest == "-":
        text = sys.stdin.read()
    elif args.manifest:
        text = Path(args.manifest).read_text()
    else:
        text = render_chart(args.chart, args.values, args.sets, os.environ.get("HELM_BIN", "helm"))
    entries = collect(yaml.safe_load_all(text))
    errors = resolve(entries, resolver) if args.resolve else []
    for error in errors:
        print(f"warning: {error}", file=sys.stderr)

    bom = build_bom(entries, chart_meta(args.chart), git_revision(ROOT), dt.datetime.now(dt.timezone.utc))
    rendered = to_markdown(bom) if args.format == "markdown" else json.dumps(bom, indent=2, ensure_ascii=False) + "\n"
    if args.output:
        Path(args.output).write_text(rendered)
    else:
        sys.stdout.write(rendered)

    unpinned = [e["image"] for e in entries if not e["digest"]]
    if args.require_digest and unpinned:
        print("error: images without digest:\n  " + "\n  ".join(unpinned), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
